// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/firewall"
)

// fwClient is the address the lock-out check sees for the next request.
type fwClient struct{ ip string }

func newFirewallConsole(t *testing.T) (*console, *fwClient) {
	t.Helper()
	requireCluster(t)
	sys, err := client.New(cluster.console, client.Options{Scheme: cluster.admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ip := &fwClient{ip: "127.0.0.1"}
	c := newConsole(t, func(cfg *Config) {
		cfg.System, cfg.SystemReader = sys, sys
		cfg.firewallHook = func(fw *firewallAPI) { fw.clientIP = func(*http.Request) string { return ip.ip } }
	})
	return c, ip
}

func fwDesired(t *testing.T) firewall.Desired {
	t.Helper()
	var cm corev1.ConfigMap
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap}, &cm); err != nil {
		return firewall.Desired{}
	}
	var d firewall.Desired
	_ = json.Unmarshal([]byte(cm.Data[firewall.DesiredKey]), &d)
	return d
}

// fwAgent reports a node agent's state for the current desired revision.
func fwAgent(t *testing.T, node string, s firewall.NodeStatus) {
	t.Helper()
	s.UpdatedAt = time.Now().UTC()
	raw, _ := json.Marshal(s)
	patch, _ := json.Marshal(map[string]any{"data": map[string]string{node: string(raw)}})
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: firewall.Namespace, Name: firewall.StatusConfigMap}}
	eventually(t, func() error {
		return cluster.admin.Patch(context.Background(), cm, client.RawPatch(types.MergePatchType, patch))
	})
}

func TestFirewallAPI(t *testing.T) {
	c, ip := newFirewallConsole(t)
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fw-api-node"}}
	if err := cluster.admin.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), node) })

	// Owners and admins only.
	if code := c.dev.do(t, "GET", "/api/v1/firewall", nil, nil); code != http.StatusForbidden {
		t.Fatalf("developer reads the firewall: %d", code)
	}
	if code := c.viewer.do(t, "POST", "/api/v1/firewall/rules", map[string]any{"name": "x", "port": 8080, "protocol": "TCP"}, nil); code != http.StatusForbidden {
		t.Fatalf("viewer writes the firewall: %d", code)
	}

	// The required rules are listed first; the node has no agent yet.
	var fw firewallJSON
	eventually(t, func() error {
		fw = firewallJSON{}
		if code := c.owner.do(t, "GET", "/api/v1/firewall", nil, &fw); code != http.StatusOK {
			return fmt.Errorf("GET: %d", code)
		}
		if len(fw.Rules) < 6 || fw.Rules[0].Name != firewall.RuleSSH || len(fw.Nodes) == 0 {
			return fmt.Errorf("rules %d, nodes %d", len(fw.Rules), len(fw.Nodes))
		}
		return nil
	})
	if fw.Rules[0].Editable != "sources" || fw.Rules[1].Editable != "none" || !fw.Rules[0].Required {
		t.Fatalf("required rules: %+v", fw.Rules[:2])
	}
	if fw.State != "no-agents" || fw.Client.Verifiable || !fw.Client.SSH {
		t.Fatalf("state %q, client %+v", fw.State, fw.Client)
	}

	// The agent runs the baseline: the console sees it in sync.
	base := fwDesired(t)
	fwAgent(t, "fw-api-node", firewall.NodeStatus{State: firewall.StateInSync, Seen: base.Revision, Confirmed: base.Revision})
	eventually(t, func() error {
		fw = firewallJSON{}
		c.owner.do(t, "GET", "/api/v1/firewall", nil, &fw)
		if fw.State != "in-sync" || fw.Confirmed != base.Revision {
			return fmt.Errorf("state %q, confirmed %q", fw.State, fw.Confirmed)
		}
		return nil
	})

	// Custom rules: validated, named, created as the user.
	var errBody struct{ Error, Field string }
	for _, tc := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"name": "ssh", "port": 8080, "protocol": "TCP"}, "name"},
		{map[string]any{"name": "Web", "port": 8080, "protocol": "TCP"}, "name"},
		{map[string]any{"name": "web", "port": 22, "protocol": "TCP"}, "port"},
		{map[string]any{"name": "web", "port": 6443, "protocol": "TCP"}, "sources"},
		{map[string]any{"name": "web", "port": 8080, "protocol": "TCP", "sources": []string{"10.0.1.3/16"}}, "sources"},
	} {
		errBody.Field = ""
		if code := c.owner.do(t, "POST", "/api/v1/firewall/rules", tc.body, &errBody); code != http.StatusBadRequest || errBody.Field != tc.field {
			t.Errorf("%v: %d %+v", tc.body, code, errBody)
		}
	}
	var created firewallRuleJSON
	if code := c.owner.do(t, "POST", "/api/v1/firewall/rules", map[string]any{"name": "fw-web", "port": 8080, "endPort": 8080, "protocol": "tcp",
		"sources": []string{"203.0.113.7"}, "description": "Staging"}, &created); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if created.EndPort != 0 || created.Protocol != "TCP" || created.Sources[0] != "203.0.113.7/32" || created.Nodes != "all" {
		t.Fatalf("created: %+v", created)
	}
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: "fw-web"}})
	})

	// Required rules: not deletable; only SSH sources change.
	if code := c.owner.do(t, "DELETE", "/api/v1/firewall/rules/ssh", nil, nil); code != http.StatusConflict {
		t.Fatalf("deleted ssh: %d", code)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/https", map[string]any{"sources": []string{"203.0.113.0/24"}}, nil); code != http.StatusConflict {
		t.Fatalf("narrowed https: %d", code)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/ssh", map[string]any{"port": 2222, "sources": []string{"203.0.113.0/24"}}, nil); code != http.StatusConflict {
		t.Fatalf("moved ssh: %d", code)
	}

	// Lock-out protection: an address Kwerft cannot verify may not narrow SSH …
	narrow := map[string]any{"sources": []string{"198.51.100.0/24"}}
	errBody.Error = ""
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/ssh", narrow, &errBody); code != http.StatusBadRequest || !strings.Contains(errBody.Error, "127.0.0.1") {
		t.Fatalf("unverifiable address: %d %+v", code, errBody)
	}
	// … nor narrow it past itself …
	ip.ip = "203.0.113.7"
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/ssh", narrow, &errBody); code != http.StatusBadRequest || !strings.Contains(errBody.Error, "203.0.113.7/32") {
		t.Fatalf("lock-out: %d %+v", code, errBody)
	}
	// … but the private network always reaches SSH …
	ip.ip = "10.0.0.5"
	var ssh firewallRuleJSON
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/ssh", narrow, &ssh); code != http.StatusOK {
		t.Fatalf("from the private network: %d", code)
	}
	// … and a range containing the address is fine.
	ip.ip = "203.0.113.7"
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/ssh", map[string]any{"sources": []string{"198.51.100.0/24", "203.0.113.0/24"}}, &ssh); code != http.StatusOK {
		t.Fatalf("narrow: %d", code)
	}
	if strings.Join(ssh.Sources, ",") != "198.51.100.0/24,203.0.113.0/24" {
		t.Fatalf("ssh: %+v", ssh)
	}

	// The agent holds the change pending.
	var changed firewall.Desired
	eventually(t, func() error {
		changed = fwDesired(t)
		if len(changed.Nodes["fw-api-node"].SSHSources) != 2 {
			return fmt.Errorf("not rendered yet")
		}
		return nil
	})
	deadline := time.Now().Add(50 * time.Second).UTC()
	fwAgent(t, "fw-api-node", firewall.NodeStatus{State: firewall.StatePending, Seen: changed.Revision, Confirmed: base.Revision, Pending: changed.Revision, Deadline: &deadline})
	eventually(t, func() error {
		fw = firewallJSON{}
		c.owner.do(t, "GET", "/api/v1/firewall", nil, &fw)
		if fw.State != "pending" || fw.Pending == nil || fw.Pending.RemainingSeconds < 40 || !fw.CanRollBack {
			return fmt.Errorf("state %q, pending %+v, canRollBack %v", fw.State, fw.Pending, fw.CanRollBack)
		}
		return nil
	})
	if !fw.Client.SSH || !fw.Client.Verifiable {
		t.Fatalf("client: %+v", fw.Client)
	}

	// "Roll back now" restores the confirmed rules: SSH open again, the new rule gone.
	var rb struct{ Changed []string }
	if code := c.owner.do(t, "POST", "/api/v1/firewall/rollback", nil, &rb); code != http.StatusOK {
		t.Fatalf("rollback: %d", code)
	}
	if strings.Join(rb.Changed, ", ") != "deleted fw-web, restored ssh" {
		t.Fatalf("rollback changed %v", rb.Changed)
	}
	eventually(t, func() error {
		var rule kwerftv1.FirewallRule
		if err := cluster.admin.Get(ctx, client.ObjectKey{Name: firewall.RuleSSH}, &rule); err != nil {
			return err
		}
		if len(rule.Spec.Sources) != 0 || fwDesired(t).Revision != base.Revision {
			return fmt.Errorf("ssh sources %v", rule.Spec.Sources)
		}
		return nil
	})

	// Again, and this time confirm it.
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/ssh", map[string]any{"sources": []string{"203.0.113.0/24"}}, nil); code != http.StatusOK {
		t.Fatalf("narrow: %d", code)
	}
	eventually(t, func() error {
		changed = fwDesired(t)
		if len(changed.Nodes["fw-api-node"].SSHSources) != 1 {
			return fmt.Errorf("not rendered yet")
		}
		return nil
	})
	if code := c.owner.do(t, "POST", "/api/v1/firewall/confirm", map[string]string{"revision": changed.Revision}, nil); code != http.StatusConflict {
		t.Fatalf("confirmed before any node applied it: %d", code)
	}
	fwAgent(t, "fw-api-node", firewall.NodeStatus{State: firewall.StatePending, Seen: changed.Revision, Confirmed: base.Revision, Pending: changed.Revision, Deadline: &deadline})
	if code := c.owner.do(t, "POST", "/api/v1/firewall/confirm", map[string]string{"revision": "stale"}, nil); code != http.StatusConflict {
		t.Fatalf("confirmed a stale revision: %d", code)
	}
	if code := c.dev.do(t, "POST", "/api/v1/firewall/confirm", map[string]string{"revision": changed.Revision}, nil); code != http.StatusForbidden {
		t.Fatalf("developer confirmed: %d", code)
	}
	if code := c.owner.do(t, "POST", "/api/v1/firewall/confirm", map[string]string{"revision": changed.Revision}, nil); code != http.StatusOK {
		t.Fatalf("confirm: %d", code)
	}
	eventually(t, func() error {
		if d := fwDesired(t); d.Confirmed != changed.Revision {
			return fmt.Errorf("confirmed %q", d.Confirmed)
		}
		return nil
	})

	// A change nobody confirmed is rolled back; "Apply again" retries it.
	if code := c.owner.do(t, "PUT", "/api/v1/firewall/rules/ssh", map[string]any{"sources": []string{"203.0.113.7"}}, nil); code != http.StatusOK {
		t.Fatalf("narrow further: %d", code)
	}
	eventually(t, func() error {
		if d := fwDesired(t); d.Revision == changed.Revision {
			return fmt.Errorf("not rendered yet")
		}
		return nil
	})
	again := fwDesired(t)
	at := time.Now().UTC()
	fwAgent(t, "fw-api-node", firewall.NodeStatus{State: firewall.StateRolledBack, Seen: again.Revision, Confirmed: changed.Revision, RolledBack: again.Revision, RolledBackAt: &at})
	eventually(t, func() error {
		fw = firewallJSON{}
		c.owner.do(t, "GET", "/api/v1/firewall", nil, &fw)
		if fw.State != "rolled-back" || fw.RolledBack == nil || fw.RolledBack.At == nil {
			return fmt.Errorf("state %q, %+v", fw.State, fw.RolledBack)
		}
		return nil
	})
	if code := c.owner.do(t, "POST", "/api/v1/firewall/confirm", map[string]string{"revision": again.Revision}, nil); code != http.StatusConflict {
		t.Fatalf("confirmed a rolled back change: %d", code)
	}
	// Applying again checks the address too.
	ip.ip = "198.51.100.9"
	if code := c.owner.do(t, "POST", "/api/v1/firewall/retry", map[string]string{"revision": again.Revision}, nil); code != http.StatusBadRequest {
		t.Fatalf("retry from an address it would block: %d", code)
	}
	ip.ip = "203.0.113.7"
	if code := c.owner.do(t, "POST", "/api/v1/firewall/retry", map[string]string{"revision": again.Revision}, nil); code != http.StatusOK {
		t.Fatalf("retry: %d", code)
	}
	eventually(t, func() error {
		if d := fwDesired(t); d.Revision == again.Revision {
			return fmt.Errorf("the retry did not make a new revision")
		}
		return nil
	})

	// Every change is in the audit log.
	entries, err := c.store.RecentAudit(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	for _, a := range []string{"firewall.rule_create", "firewall.rule_update", "firewall.lockout_refused", "firewall.rollback", "firewall.confirm", "firewall.retry"} {
		if !seen[a] {
			t.Errorf("no %s in the audit log", a)
		}
	}

	// Put SSH back for other tests.
	var rule kwerftv1.FirewallRule
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: firewall.RuleSSH}, &rule); err == nil {
		rule.Spec.Sources, rule.Annotations = nil, nil
		_ = cluster.admin.Update(ctx, &rule)
	}
}
