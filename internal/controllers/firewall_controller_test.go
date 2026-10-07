// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/firewall"
)

const testPrivateNetwork = "10.0.0.0/16"

func readDesired(t *testing.T) (firewall.Desired, FirewallSnapshot) {
	t.Helper()
	var cm corev1.ConfigMap
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap}, &cm); err != nil {
		t.Fatalf("desired ConfigMap: %v", err)
	}
	var d firewall.Desired
	var s FirewallSnapshot
	_ = json.Unmarshal([]byte(cm.Data[firewall.DesiredKey]), &d)
	_ = json.Unmarshal([]byte(cm.Data[firewall.SnapshotKey]), &s)
	return d, s
}

func desiredNow(t *testing.T, check func(firewall.Desired, FirewallSnapshot) error) firewall.Desired {
	t.Helper()
	var d firewall.Desired
	eventually(t, func() error {
		var cm corev1.ConfigMap
		if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap}, &cm); err != nil {
			return err
		}
		var s FirewallSnapshot
		d = firewall.Desired{}
		_ = json.Unmarshal([]byte(cm.Data[firewall.DesiredKey]), &d)
		_ = json.Unmarshal([]byte(cm.Data[firewall.SnapshotKey]), &s)
		return check(d, s)
	})
	return d
}

// agentReport writes a node agent's status, as the agent would.
func agentReport(t *testing.T, node string, s firewall.NodeStatus) {
	t.Helper()
	s.UpdatedAt = time.Now().UTC()
	raw, _ := json.Marshal(s)
	patch, _ := json.Marshal(map[string]any{"data": map[string]string{node: string(raw)}})
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: firewall.Namespace, Name: firewall.StatusConfigMap}}
	eventually(t, func() error { return k8s.Patch(context.Background(), cm, client.RawPatch(types.MergePatchType, patch)) })
}

func fwReady(t *testing.T, name, reason string) {
	t.Helper()
	eventually(t, func() error {
		var r kwerftv1.FirewallRule
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &r); err != nil {
			return err
		}
		got, err := readyReason(r.Status.Conditions, r.Generation)
		if err != nil {
			return err
		}
		if got != reason {
			return fmt.Errorf("%s: Ready reason %s, want %s", name, got, reason)
		}
		return nil
	})
}

func TestFirewallRules(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	// Required rules appear, labelled, with the private network.
	eventually(t, func() error {
		for _, want := range firewall.Required(testPrivateNetwork) {
			var r kwerftv1.FirewallRule
			if err := k8s.Get(ctx, client.ObjectKey{Name: want.Name}, &r); err != nil {
				return err
			}
			if !firewall.IsRequired(&r) {
				return fmt.Errorf("%s is not labelled required", r.Name)
			}
		}
		return nil
	})
	fwReady(t, firewall.RuleHTTPS, "Baseline")

	// A hand edit of a required rule is put back; only SSH sources stay.
	var https kwerftv1.FirewallRule
	if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleHTTPS}, &https); err != nil {
		t.Fatal(err)
	}
	https.Spec.Sources = []string{"203.0.113.0/24"}
	if err := k8s.Update(ctx, &https); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleHTTPS}, &https); err != nil {
			return err
		}
		if len(https.Spec.Sources) != 0 {
			return fmt.Errorf("https sources still %v", https.Spec.Sources)
		}
		return nil
	})
	// A deleted required rule comes back.
	if err := k8s.Delete(ctx, &https); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var r kwerftv1.FirewallRule
		if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleHTTPS}, &r); err != nil {
			return err
		}
		if r.UID == https.UID {
			return fmt.Errorf("not deleted yet")
		}
		return nil
	})

	// Two nodes, one of them a control-plane node.
	for _, n := range []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "fw-cp", Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fw-worker"}},
	} {
		if err := k8s.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8s.Delete(context.Background(), n) })
	}
	base := desiredNow(t, func(d firewall.Desired, _ FirewallSnapshot) error {
		if _, ok := d.Nodes["fw-worker"]; !ok {
			return fmt.Errorf("nodes: %v", d.Nodes)
		}
		return nil
	})

	// Both agents run the baseline: the revision counts as confirmed and
	// the rules are snapshotted for "Roll back".
	for _, n := range []string{"fw-cp", "fw-worker"} {
		agentReport(t, n, firewall.NodeStatus{State: firewall.StateInSync, Seen: base.Revision, Confirmed: base.Revision})
	}
	desiredNow(t, func(d firewall.Desired, s FirewallSnapshot) error {
		if d.Confirmed != base.Revision || s.Revision != base.Revision || len(s.Rules) < 6 {
			return fmt.Errorf("confirmed %q, snapshot %q with %d rules (revision %q)", d.Confirmed, s.Revision, len(s.Rules), base.Revision)
		}
		return nil
	})

	// A control-plane-only rule reaches only the control-plane node.
	api := &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: "fw-test-api"},
		Spec: kwerftv1.FirewallRuleSpec{Port: 6443, Protocol: "TCP", Sources: []string{"198.51.100.0/24"}, Nodes: "control-plane"}}
	if err := k8s.Create(ctx, api); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), api) })
	opened := desiredNow(t, func(d firewall.Desired, _ FirewallSnapshot) error {
		if len(d.Nodes["fw-cp"].Open) != 1 || len(d.Nodes["fw-worker"].Open) != 0 {
			return fmt.Errorf("nodes: %+v", d.Nodes)
		}
		return nil
	})
	if opened.Revision == base.Revision || opened.Confirmed != base.Revision {
		t.Fatalf("revision %s (confirmed %s)", opened.Revision, opened.Confirmed)
	}
	fwReady(t, "fw-test-api", "Applying")

	// The control-plane agent holds it pending, the worker has nothing new.
	deadline := time.Now().Add(time.Minute).UTC()
	agentReport(t, "fw-cp", firewall.NodeStatus{State: firewall.StatePending, Seen: opened.Revision, Confirmed: base.Revision, Pending: opened.Revision, Deadline: &deadline})
	agentReport(t, "fw-worker", firewall.NodeStatus{State: firewall.StateInSync, Seen: opened.Revision, Confirmed: opened.Revision})
	fwReady(t, "fw-test-api", "PendingConfirmation")
	if d, _ := readDesired(t); d.Confirmed != base.Revision {
		t.Fatalf("confirmed without anyone confirming: %s", d.Confirmed)
	}

	// An owner confirms (the console writes the annotation as that user).
	var ssh kwerftv1.FirewallRule
	if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleSSH}, &ssh); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(ssh.DeepCopy())
	ssh.Annotations = map[string]string{firewall.AnnotationConfirmed: opened.Revision}
	if err := k8s.Patch(ctx, &ssh, patch); err != nil {
		t.Fatal(err)
	}
	desiredNow(t, func(d firewall.Desired, s FirewallSnapshot) error {
		if d.Confirmed != opened.Revision || s.Revision != opened.Revision {
			return fmt.Errorf("confirmed %q, snapshot %q", d.Confirmed, s.Revision)
		}
		return nil
	})
	agentReport(t, "fw-cp", firewall.NodeStatus{State: firewall.StateInSync, Seen: opened.Revision, Confirmed: opened.Revision})
	fwReady(t, "fw-test-api", "Applied")

	// An invalid rule written past the console is reported, not rendered.
	bad := &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: "fw-test-bad"},
		Spec: kwerftv1.FirewallRuleSpec{Port: 6443, Protocol: "TCP"}} // the API to everyone
	if err := k8s.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), bad) })
	fwReady(t, "fw-test-bad", "Invalid")
	if d, _ := readDesired(t); d.Revision != opened.Revision {
		t.Fatal("an invalid rule changed the revision")
	}

	// A rolled back revision is not confirmed by a late confirmation.
	narrow := ssh.DeepCopy()
	if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleSSH}, narrow); err != nil {
		t.Fatal(err)
	}
	narrow.Spec.Sources = []string{"203.0.113.0/24"}
	if err := k8s.Update(ctx, narrow); err != nil {
		t.Fatal(err)
	}
	narrowed := desiredNow(t, func(d firewall.Desired, _ FirewallSnapshot) error {
		if len(d.Nodes["fw-worker"].SSHSources) != 1 {
			return fmt.Errorf("not narrowed yet")
		}
		return nil
	})
	for _, n := range []string{"fw-cp", "fw-worker"} {
		agentReport(t, n, firewall.NodeStatus{State: firewall.StateRolledBack, Seen: narrowed.Revision, Confirmed: opened.Revision, RolledBack: narrowed.Revision})
	}
	fwReady(t, firewall.RuleSSH, "RolledBack")
	if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleSSH}, narrow); err != nil {
		t.Fatal(err)
	}
	patch = client.MergeFrom(narrow.DeepCopy())
	narrow.Annotations[firewall.AnnotationConfirmed] = narrowed.Revision
	if err := k8s.Patch(ctx, narrow, patch); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if d, s := readDesired(t); d.Confirmed != opened.Revision || s.Revision != opened.Revision {
		t.Fatalf("a rolled back revision was confirmed: %s / %s", d.Confirmed, s.Revision)
	}

	// Put SSH back for other tests.
	if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleSSH}, narrow); err != nil {
		t.Fatal(err)
	}
	narrow.Spec.Sources = nil
	narrow.Annotations = nil
	if err := k8s.Update(ctx, narrow); err != nil {
		t.Fatal(err)
	}
}
