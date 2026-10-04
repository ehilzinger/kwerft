package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hubble"
)

type trafficResp struct {
	Project  string `json:"project"`
	Isolated bool   `json:"isolated"`
	Hubble   struct {
		State   string    `json:"state"`
		Message string    `json:"message"`
		Since   time.Time `json:"since"`
	} `json:"hubble"`
	Rules []struct {
		Name     string                  `json:"name"`
		Phase    string                  `json:"phase"`
		Reason   string                  `json:"reason"`
		Message  string                  `json:"message"`
		Policies []string                `json:"policies"`
		Counts   *kwerftv1.TrafficCounts `json:"counts"`
		From     []kwerftv1.TrafficPeer  `json:"from"`
	} `json:"rules"`
	Drops []dropResp `json:"drops"`
}

type dropResp struct {
	From       hubble.Side        `json:"from"`
	To         hubble.Side        `json:"to"`
	Port       uint32             `json:"port"`
	Count      int64              `json:"count"`
	Suggestion *hubble.Suggestion `json:"suggestion"`
}

func ep(ns, app string) hubble.Endpoint {
	return hubble.Endpoint{Namespace: ns, Pod: app + "-abc", IP: "10.42.1.9", Labels: []string{"k8s:kwerft.dev/app=" + app}}
}

func policyDrop(from, to hubble.Endpoint, port uint32) *hubble.Flow {
	return &hubble.Flow{Time: time.Now(), Verdict: hubble.VerdictDropped, DropReason: hubble.DropPolicyDenied,
		EventType: hubble.EventDrop, Source: from, Destination: to, Protocol: "TCP", DstPort: port, Direction: hubble.DirectionIngress}
}

func TestTrafficRulesThroughTheAPI(t *testing.T) {
	flows := hubble.NewAggregator()
	c := newConsole(t, func(cfg *Config) { cfg.Hubble = flows })
	c.project(t, "net-shop")
	c.project(t, "net-other")

	rule := map[string]any{
		"name":  "web-to-api",
		"from":  []map[string]any{{"app": "web"}},
		"to":    []map[string]any{{"app": "api"}},
		"ports": []map[string]any{{"port": 8080}},
	}
	// Viewers read, developers manage (RBAC).
	if code := c.viewer.do(t, "POST", "/api/v1/projects/net-shop/trafficrules", rule, nil); code != http.StatusForbidden {
		t.Errorf("viewer create: %d, want 403", code)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/net-shop/trafficrules", rule, nil); code != http.StatusCreated {
		t.Fatalf("developer create: %d", code)
	}

	// Invalid rules name the field.
	var e apiError
	bad := map[string]any{"name": "out", "from": []map[string]any{{"app": "net-other/x"}}, "to": []map[string]any{{"internet": true}}}
	if code := c.dev.do(t, "POST", "/api/v1/projects/net-shop/trafficrules", bad, &e); code != http.StatusUnprocessableEntity || e.Field != "from[0]" {
		t.Errorf("egress from another project: %d %+v", code, e)
	}
	bad = map[string]any{"name": "Bad Name", "from": []map[string]any{{"app": "web"}}, "to": []map[string]any{{"app": "api"}}}
	if code := c.dev.do(t, "POST", "/api/v1/projects/net-shop/trafficrules", bad, &e); code != http.StatusUnprocessableEntity || e.Field != "name" {
		t.Errorf("bad name: %d %+v", code, e)
	}

	// The reconciler applies it; the app does not exist yet.
	var tr trafficResp
	eventually(t, func() error {
		if code := c.viewer.do(t, "GET", "/api/v1/projects/net-shop/traffic", nil, &tr); code != http.StatusOK {
			return fmt.Errorf("overview: %d", code)
		}
		if len(tr.Rules) != 1 || tr.Rules[0].Phase != "waiting" || tr.Rules[0].Reason != "AppNotFound" {
			return fmt.Errorf("rules = %+v", tr.Rules)
		}
		return nil
	})
	if !tr.Isolated || tr.Hubble.State != "connecting" || len(tr.Rules[0].Policies) != 1 {
		t.Errorf("overview = %+v", tr)
	}

	// Hubble: an allowed connection and drops, in this and another project.
	in := hubble.Policy{Name: "web-to-api.traffic-in", Namespace: "net-shop"}
	flows.Add(&hubble.Flow{Time: time.Now(), Verdict: hubble.VerdictForwarded, EventType: hubble.EventPolicyVerdict,
		Source: ep("net-shop", "web"), Destination: ep("net-shop", "api"), Protocol: "TCP", DstPort: 8080, AllowedBy: []hubble.Policy{in}})
	flows.Add(policyDrop(ep("net-shop", "web"), ep("net-shop", "api"), 9090))
	flows.Add(policyDrop(ep("net-shop", "worker"), ep("net-shop", "api"), 8080))
	flows.Add(policyDrop(ep("net-other", "x"), ep("net-other", "y"), 80))
	flows.Add(policyDrop(ep("kube-system", "coredns"), ep("kwerft-system", "kwerft"), 8081))

	if code := c.dev.do(t, "GET", "/api/v1/projects/net-shop/traffic", nil, &tr); code != http.StatusOK {
		t.Fatalf("overview: %d", code)
	}
	if tr.Hubble.State != "ok" {
		t.Errorf("hubble = %+v", tr.Hubble)
	}
	if c := tr.Rules[0].Counts; c == nil || c.Allowed != 1 || c.Dropped != 1 {
		t.Errorf("counts = %+v, want 1 allowed, 1 dropped (web → api on 9090)", c)
	}
	if len(tr.Drops) != 2 {
		t.Fatalf("drops = %+v, want net-shop's two", tr.Drops)
	}
	worker := tr.Drops[slices.IndexFunc(tr.Drops, func(d dropResp) bool {
		return d.From.App == "worker"
	})]
	if s := worker.Suggestion; s == nil || s.Project != "net-shop" || s.Spec.From[0].App != "worker" || s.Spec.To[0].App != "api" || s.Spec.Ports[0].Port != 8080 {
		t.Errorf("suggestion = %+v", worker.Suggestion)
	}

	// The cluster-wide list: projects for developers, everything for owners.
	var all struct {
		Drops []struct {
			From hubble.Side `json:"from"`
		} `json:"drops"`
	}
	if code := c.dev.do(t, "GET", "/api/v1/traffic/drops", nil, &all); code != http.StatusOK || len(all.Drops) != 3 {
		t.Errorf("developer drops: %d %+v, want the 3 of projects", code, all.Drops)
	}
	for _, d := range all.Drops {
		if d.From.Namespace == "kube-system" {
			t.Errorf("a developer sees platform traffic: %+v", d)
		}
	}
	if code := c.owner.do(t, "GET", "/api/v1/traffic/drops", nil, &all); code != http.StatusOK || len(all.Drops) != 4 {
		t.Errorf("owner drops: %d %d, want 4", code, len(all.Drops))
	}

	// Update, then delete; both audited.
	rule["ports"] = []map[string]any{{"port": 8080}, {"port": 9090}}
	if code := c.dev.do(t, "PUT", "/api/v1/projects/net-shop/trafficrules/web-to-api", rule, nil); code != http.StatusOK {
		t.Errorf("update: %d", code)
	}
	var stored kwerftv1.TrafficRule
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: "net-shop", Name: "web-to-api"}, &stored); err != nil || len(stored.Spec.Ports) != 2 {
		t.Errorf("stored rule = %+v, %v", stored.Spec, err)
	}
	if code := c.viewer.do(t, "DELETE", "/api/v1/projects/net-shop/trafficrules/web-to-api", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer delete: %d", code)
	}
	if code := c.dev.do(t, "DELETE", "/api/v1/projects/net-shop/trafficrules/web-to-api", nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	entries, err := c.store.RecentAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range entries {
		if e.Target == "net-shop/web-to-api" {
			actions = append(actions, e.Action)
		}
	}
	for _, want := range []string{"trafficrule.create", "trafficrule.update", "trafficrule.delete", "trafficrule.delete.denied"} {
		if !slices.Contains(actions, want) {
			t.Errorf("audit lacks %s: %v", want, actions)
		}
	}
}

func TestProjectIsolationToggle(t *testing.T) {
	c := newConsole(t)
	c.project(t, "net-iso")
	if code := c.dev.do(t, "PUT", "/api/v1/projects/net-iso/isolation", map[string]bool{"isolated": false}, nil); code != http.StatusForbidden {
		t.Errorf("developer: %d, want 403 (projects are owners' and admins')", code)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/projects/net-iso/isolation", map[string]bool{"isolated": false}, nil); code != http.StatusOK {
		t.Fatalf("owner: %d", code)
	}
	var tr trafficResp
	if code := c.viewer.do(t, "GET", "/api/v1/projects/net-iso/traffic", nil, &tr); code != http.StatusOK || tr.Isolated {
		t.Errorf("overview: %d isolated=%v", code, tr.Isolated)
	}
	// No Hubble on this console.
	if tr.Hubble.State != "off" || len(tr.Drops) != 0 {
		t.Errorf("hubble = %+v drops %d", tr.Hubble, len(tr.Drops))
	}
	if code := c.owner.do(t, "PUT", "/api/v1/projects/net-iso/isolation", map[string]any{}, nil); code != http.StatusUnprocessableEntity {
		t.Errorf("missing value: %d", code)
	}
	if code := c.viewer.do(t, "GET", "/api/v1/projects/nope/traffic", nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown project: %d", code)
	}
}
