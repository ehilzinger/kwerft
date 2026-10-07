// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// ---- helpers -----------------------------------------------------------------

func newCilium() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CiliumNetworkPolicyGVK)
	return u
}

// ciliumSpec waits for a CiliumNetworkPolicy and returns its spec.
func ciliumSpec(t *testing.T, namespace, name string) map[string]any {
	t.Helper()
	u := newCilium()
	eventually(t, func() error {
		return k8s.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, u)
	})
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	return spec
}

// assertSpec compares a rendered spec with the YAML that says what it must be.
func assertSpec(t *testing.T, got map[string]any, want string) {
	t.Helper()
	var w any
	if err := yaml.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad expectation: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var g any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		gy, _ := yaml.Marshal(g)
		wy, _ := yaml.Marshal(w)
		t.Errorf("policy spec differs\n--- got\n%s--- want\n%s", gy, wy)
	}
}

func specOf(u *unstructured.Unstructured) map[string]any {
	if u == nil {
		return nil
	}
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	return spec
}

// apiPolicy is what the App "api" of TestAppRendersDeploymentServiceRouteAndPolicy
// (project shop, port 8080, allowFrom web-frontend and internal/cron,
// egress https) must become: exactly the Kubernetes NetworkPolicy Kwerft
// wrote before Phase 4, plus the nodes, which that one could not name.
const apiPolicy = `
endpointSelector:
  matchLabels: {kwerft.dev/app: api}
ingress:
- fromEntities: [host, remote-node]
  toPorts: [{ports: [{port: "8080", protocol: TCP}]}]
- fromEndpoints:
  - matchLabels: {k8s:io.cilium.k8s.namespace.labels.kwerft.dev/system: "true"}
    matchExpressions: [{key: k8s:io.kubernetes.pod.namespace, operator: Exists}]
  - matchLabels: {k8s:io.kubernetes.pod.namespace: shop, kwerft.dev/app: web-frontend}
  - matchLabels: {k8s:io.kubernetes.pod.namespace: shop, kwerft.dev/as-app: web-frontend}
  - matchLabels: {k8s:io.kubernetes.pod.namespace: internal, kwerft.dev/app: cron}
  - matchLabels: {k8s:io.kubernetes.pod.namespace: internal, kwerft.dev/as-app: cron}
  toPorts: [{ports: [{port: "8080", protocol: TCP}]}]
egress:
- toEndpoints: [{matchLabels: {k8s:io.kubernetes.pod.namespace: kube-system, k8s-app: kube-dns}}]
  toPorts: [{ports: [{port: "53", protocol: UDP}, {port: "53", protocol: TCP}]}]
- toEndpoints: [{matchExpressions: [{key: k8s:io.kubernetes.pod.namespace, operator: Exists}]}]
- toCIDRSet: [{cidr: 0.0.0.0/0, except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16]}]
  toPorts: [{ports: [{port: "443", protocol: TCP}]}]
- toEntities: [host, remote-node]
  toPorts: [{ports: [{port: "443", protocol: TCP}]}]
`

// ---- rendering (no API server) -----------------------------------------------

// Egress https reaches the cluster's own hostnames: they resolve to a node,
// which Cilium calls host (this node) or remote-node, never a CIDR, so the
// internet rule alone dropped the hairpin (hatchure's gh-activate Task timed
// out on https://router.hatchure.app, 2026-10-06). Only 443 on the nodes,
// Traefik's port; none stays closed and all has no egress policy. The same
// for Apps and Tasks.
func TestEgressReachesTheClustersOwnIngress(t *testing.T) {
	nodes443 := map[string]any{
		"toEntities": []any{"host", "remote-node"},
		"toPorts":    []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}},
	}
	for _, tc := range []struct {
		egress   string
		rules    int  // egress rules; 0: no egress section
		nodes443 bool // the rule above is one of them
	}{
		{"", 4, true}, {"https", 4, true}, {"none", 2, false}, {"all", 0, false},
	} {
		app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: kwerftv1.AppSpec{Egress: tc.egress}}
		run := &taskRun{task: &kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Name: "check", Namespace: "shop"}}, project: "shop", egress: tc.egress}
		for kind, spec := range map[string]map[string]any{
			"App":  specOf(newAppRender(app, "img", "shop").ciliumPolicy(true)),
			"Task": specOf(run.ciliumPolicy()),
		} {
			rules, _ := spec["egress"].([]any)
			_, hasEgress := spec["egress"]
			if len(rules) != tc.rules || hasEgress != (tc.rules > 0) {
				t.Errorf("%s egress %q: %d egress rules (section %v), want %d", kind, tc.egress, len(rules), hasEgress, tc.rules)
			}
			found := false
			for _, r := range rules {
				m := r.(map[string]any)
				if _, ok := m["toEntities"]; ok {
					found = true
					if !reflect.DeepEqual(m, nodes443) {
						t.Errorf("%s egress %q: node rule %v, want only 443 on host and remote-node", kind, tc.egress, m)
					}
				}
			}
			if found != tc.nodes443 {
				t.Errorf("%s egress %q: reaches the nodes = %v, want %v", kind, tc.egress, found, tc.nodes443)
			}
		}
	}

	// A Task never accepts connections.
	run := &taskRun{task: &kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Name: "check", Namespace: "shop"}}, project: "shop", egress: "https"}
	if in := specOf(run.ciliumPolicy())["ingress"]; !reflect.DeepEqual(in, []any{map[string]any{}}) {
		t.Errorf("Task ingress = %v, want default deny ([{}])", in)
	}
}

func TestAppPolicyEgressAndIsolation(t *testing.T) {
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "shop"}, Spec: kwerftv1.AppSpec{Egress: "none"}}
	rd := newAppRender(app, "img", "shop")

	// No ports: every port, as a NetworkPolicy without ports did. Egress
	// none: DNS and the cluster only.
	assertSpec(t, specOf(rd.ciliumPolicy(true)), `
endpointSelector: {matchLabels: {kwerft.dev/app: worker}}
ingress:
- fromEntities: [host, remote-node]
- fromEndpoints:
  - matchLabels: {k8s:io.cilium.k8s.namespace.labels.kwerft.dev/system: "true"}
    matchExpressions: [{key: k8s:io.kubernetes.pod.namespace, operator: Exists}]
egress:
- toEndpoints: [{matchLabels: {k8s:io.kubernetes.pod.namespace: kube-system, k8s-app: kube-dns}}]
  toPorts: [{ports: [{port: "53", protocol: UDP}, {port: "53", protocol: TCP}]}]
- toEndpoints: [{matchExpressions: [{key: k8s:io.kubernetes.pod.namespace, operator: Exists}]}]
`)

	// Egress all: no egress section (no default deny). Not isolated: every
	// project's pods may connect.
	app.Spec.Egress = "all"
	assertSpec(t, specOf(rd.ciliumPolicy(false)), `
endpointSelector: {matchLabels: {kwerft.dev/app: worker}}
ingress:
- fromEntities: [host, remote-node]
- fromEndpoints:
  - matchLabels: {k8s:io.cilium.k8s.namespace.labels.kwerft.dev/system: "true"}
    matchExpressions: [{key: k8s:io.kubernetes.pod.namespace, operator: Exists}]
  - matchExpressions:
    - {key: k8s:io.cilium.k8s.namespace.labels.kwerft.dev/project, operator: Exists}
    - {key: k8s:io.kubernetes.pod.namespace, operator: Exists}
`)
}

func TestTrafficRulePolicies(t *testing.T) {
	rule := func(spec kwerftv1.TrafficRuleSpec) *kwerftv1.TrafficRule {
		return &kwerftv1.TrafficRule{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "shop", UID: "u1"}, Spec: spec}
	}
	tcp := []kwerftv1.TrafficPort{{Port: 5432}, {Port: 6379, Protocol: "TCP"}, {Port: 9000, EndPort: 9010, Protocol: "UDP"}}

	// Ingress: apps of this project receive from anything.
	in, out := trafficPolicies(rule(kwerftv1.TrafficRuleSpec{
		From:  []kwerftv1.TrafficPeer{{App: "api"}, {App: "internal/cron"}, {Project: "staging"}, {Internet: true}, {CIDR: "10.0.0.0/16"}},
		To:    []kwerftv1.TrafficPeer{{App: "postgres"}, {App: "shop/redis"}},
		Ports: tcp,
	}), "shop")
	if out != nil {
		t.Errorf("an ingress-only rule rendered an egress policy: %v", specOf(out))
	}
	if in.GetName() != "r.traffic-in" || in.GetLabels()[LabelTrafficRule] != "r" || in.GetOwnerReferences()[0].Kind != "TrafficRule" {
		t.Errorf("metadata = %s %v %v", in.GetName(), in.GetLabels(), in.GetOwnerReferences())
	}
	ports := `[{ports: [{port: "5432", protocol: TCP}, {port: "6379", protocol: TCP}, {port: "9000", endPort: 9010, protocol: UDP}]}]`
	assertSpec(t, specOf(in), `
endpointSelector:
  matchExpressions: [{key: kwerft.dev/app, operator: In, values: [postgres, redis]}]
enableDefaultDeny: {ingress: false, egress: false}
ingress:
- fromEndpoints:
  - matchLabels: {k8s:io.kubernetes.pod.namespace: shop, kwerft.dev/app: api}
  - matchLabels: {k8s:io.kubernetes.pod.namespace: shop, kwerft.dev/as-app: api}
  toPorts: `+ports+`
- fromEndpoints:
  - matchLabels: {k8s:io.kubernetes.pod.namespace: internal, kwerft.dev/app: cron}
  - matchLabels: {k8s:io.kubernetes.pod.namespace: internal, kwerft.dev/as-app: cron}
  toPorts: `+ports+`
- fromEndpoints: [{matchLabels: {k8s:io.kubernetes.pod.namespace: staging}}]
  toPorts: `+ports+`
- fromEntities: [host, remote-node, world]
  toPorts: `+ports+`
- fromCIDRSet: [{cidr: 10.0.0.0/16}]
  toPorts: `+ports+`
`)

	// Egress: the project's apps send out; no ports means every port. The
	// whole project as destination selects its apps, not every pod.
	in, out = trafficPolicies(rule(kwerftv1.TrafficRuleSpec{
		From: []kwerftv1.TrafficPeer{{App: "api"}},
		To:   []kwerftv1.TrafficPeer{{Internet: true}, {CIDR: "192.168.0.0/24"}, {App: "billing/ledger"}, {Project: "search"}, {Project: "shop"}},
	}), "shop")
	assertSpec(t, specOf(in), `
endpointSelector: {matchExpressions: [{key: kwerft.dev/app, operator: Exists}]}
enableDefaultDeny: {ingress: false, egress: false}
ingress:
- fromEndpoints:
  - matchLabels: {k8s:io.kubernetes.pod.namespace: shop, kwerft.dev/app: api}
  - matchLabels: {k8s:io.kubernetes.pod.namespace: shop, kwerft.dev/as-app: api}
`)
	assertSpec(t, specOf(out), `
endpointSelector:
  matchExpressions: [{key: kwerft.dev/app, operator: In, values: [api]}]
enableDefaultDeny: {ingress: false, egress: false}
egress:
- toCIDRSet: [{cidr: 0.0.0.0/0, except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16]}]
- toCIDRSet: [{cidr: 192.168.0.0/24}]
- toEndpoints: [{matchLabels: {k8s:io.kubernetes.pod.namespace: billing, kwerft.dev/app: ledger}}]
- toEndpoints: [{matchLabels: {k8s:io.kubernetes.pod.namespace: search}}]
`)
	if out.GetName() != "r.traffic-out" {
		t.Errorf("egress policy name = %s", out.GetName())
	}
}

func TestValidateTrafficRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  kwerftv1.TrafficRuleSpec
		field string // empty: valid
	}{
		{"ingress from anywhere", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{Internet: true}, {App: "other/x"}}, To: []kwerftv1.TrafficPeer{{App: "api"}}}, ""},
		{"egress from own apps", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{App: "shop/api"}, {Project: "shop"}}, To: []kwerftv1.TrafficPeer{{Internet: true}}}, ""},
		{"no sources", kwerftv1.TrafficRuleSpec{To: []kwerftv1.TrafficPeer{{App: "api"}}}, "spec.from"},
		{"two fields", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{App: "a", Internet: true}}, To: []kwerftv1.TrafficPeer{{App: "api"}}}, "spec.from[0]"},
		{"bad app", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{App: "a/b/c"}}, To: []kwerftv1.TrafficPeer{{App: "api"}}}, "spec.from[0].app"},
		{"bad app name", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{App: "Api"}}, To: []kwerftv1.TrafficPeer{{App: "api"}}}, "spec.from[0].app"},
		{"bad cidr", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{CIDR: "10.0.0.0"}}, To: []kwerftv1.TrafficPeer{{App: "api"}}}, "spec.from[0].cidr"},
		{"host bits", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{CIDR: "10.0.0.1/16"}}, To: []kwerftv1.TrafficPeer{{App: "api"}}}, "spec.from[0].cidr"},
		{"egress from elsewhere", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{App: "api"}, {App: "other/x"}}, To: []kwerftv1.TrafficPeer{{Internet: true}}}, "spec.from[1]"},
		{"internet to internet", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{Internet: true}}, To: []kwerftv1.TrafficPeer{{Internet: true}}}, "spec.from[0]"},
		{"to another project from the internet", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{Internet: true}}, To: []kwerftv1.TrafficPeer{{App: "other/x"}}}, "spec.from[0]"},
		{"port range backwards", kwerftv1.TrafficRuleSpec{
			From: []kwerftv1.TrafficPeer{{App: "a"}}, To: []kwerftv1.TrafficPeer{{App: "api"}}, Ports: []kwerftv1.TrafficPort{{Port: 10, EndPort: 5}}}, "spec.ports[0].endPort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTrafficRule("shop", &tc.spec)
			var fe *TrafficFieldError
			switch {
			case tc.field == "" && err != nil:
				t.Errorf("unexpected error %v", err)
			case tc.field != "" && !errors.As(err, &fe):
				t.Errorf("err = %v, want one on %s", err, tc.field)
			case tc.field != "" && fe.Field != tc.field:
				t.Errorf("field = %s (%s), want %s", fe.Field, fe.Message, tc.field)
			}
		})
	}
}

// ---- reconciler (envtest) ----------------------------------------------------

func createRule(t *testing.T, ns, name string, spec kwerftv1.TrafficRuleSpec) *kwerftv1.TrafficRule {
	t.Helper()
	r := &kwerftv1.TrafficRule{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func waitForRule(t *testing.T, r *kwerftv1.TrafficRule, wantReason string) *kwerftv1.TrafficRule {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(r), r); err != nil {
			return err
		}
		reason, err := readyReason(r.Status.Conditions, r.Generation)
		if err != nil {
			return err
		}
		if reason != wantReason {
			c := meta.FindStatusCondition(r.Status.Conditions, ConditionReady)
			return fmt.Errorf("reason %q (%s), want %q", reason, c.Message, wantReason)
		}
		return nil
	})
	return r
}

func ruleMessage(r *kwerftv1.TrafficRule) string {
	return meta.FindStatusCondition(r.Status.Conditions, ConditionReady).Message
}

func TestTrafficRuleLifecycle(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "tr-shop")
	projectNamespace(t, "tr-billing")
	img := kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.29"}}
	createApp(t, "tr-shop", "web", kwerftv1.AppSpec{Source: img, Ports: []kwerftv1.AppPort{{Container: 80}}})

	// A rule naming an app that does not exist yet is applied and says so.
	r := createRule(t, "tr-shop", "web-to-api", kwerftv1.TrafficRuleSpec{
		From:  []kwerftv1.TrafficPeer{{App: "web"}},
		To:    []kwerftv1.TrafficPeer{{App: "api"}},
		Ports: []kwerftv1.TrafficPort{{Port: 8080}},
	})
	r = waitForRule(t, r, "AppNotFound")
	if !strings.Contains(ruleMessage(r), "api") {
		t.Errorf("message = %q", ruleMessage(r))
	}
	in, _ := TrafficPolicyNames("web-to-api")
	ciliumSpec(t, "tr-shop", in)
	createApp(t, "tr-shop", "api", kwerftv1.AppSpec{Source: img, Ports: []kwerftv1.AppPort{{Container: 8080}}})
	r = waitForRule(t, r, "Applied")
	if len(r.Status.Policies) != 1 || r.Status.Policies[0] != in {
		t.Errorf("status.policies = %v", r.Status.Policies)
	}
	if got := ruleMessage(r); got != "Allows web to api on TCP 8080" {
		t.Errorf("message = %q", got)
	}

	// Disabled: the policy goes, the rule stays.
	r.Spec.Disabled = true
	if err := k8s.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	r = waitForRule(t, r, "Disabled")
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "tr-shop", Name: in}, newCilium()); !apierrors.IsNotFound(err) {
		t.Errorf("disabled rule's policy: err = %v, want gone", err)
	}

	// Invalid (the CRD cannot tell): nothing applied, the field named.
	r.Spec.Disabled = false
	r.Spec.From = []kwerftv1.TrafficPeer{{App: "tr-billing/ledger"}}
	r.Spec.To = []kwerftv1.TrafficPeer{{Internet: true}}
	if err := k8s.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	r = waitForRule(t, r, "InvalidRule")
	if !strings.Contains(ruleMessage(r), "spec.from[0]") || len(r.Status.Policies) != 0 {
		t.Errorf("invalid rule: message %q, policies %v", ruleMessage(r), r.Status.Policies)
	}
}

func TestTrafficRuleAcrossProjectsNeedsTheReceiver(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "xp-a")
	projectNamespace(t, "xp-b")
	img := kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.29"}}
	createApp(t, "xp-a", "client", kwerftv1.AppSpec{Source: img})
	createApp(t, "xp-b", "server", kwerftv1.AppSpec{Source: img, Ports: []kwerftv1.AppPort{{Container: 9000}}})

	// A's rule lets client send; B has not agreed.
	send := createRule(t, "xp-a", "to-server", kwerftv1.TrafficRuleSpec{
		From: []kwerftv1.TrafficPeer{{App: "client"}},
		To:   []kwerftv1.TrafficPeer{{App: "xp-b/server"}},
	})
	send = waitForRule(t, send, "AwaitingPeer")
	if msg := ruleMessage(send); !strings.Contains(msg, "xp-b") || !strings.Contains(msg, "client") {
		t.Errorf("message = %q", msg)
	}
	_, out := TrafficPolicyNames("to-server")
	ciliumSpec(t, "xp-a", out)
	// Nothing was written in B on A's behalf.
	var inB unstructured.UnstructuredList
	inB.SetGroupVersionKind(CiliumNetworkPolicyGVK.GroupVersion().WithKind("CiliumNetworkPolicyList"))
	if err := k8s.List(ctx, &inB, client.InNamespace("xp-b"), client.MatchingLabels{LabelTrafficRule: "to-server"}); err != nil {
		t.Fatal(err)
	}
	if len(inB.Items) != 0 {
		t.Errorf("a rule in xp-a wrote %d policies in xp-b", len(inB.Items))
	}

	// B agrees with a rule of its own; A's rule becomes Applied.
	recv := createRule(t, "xp-b", "from-client", kwerftv1.TrafficRuleSpec{
		From: []kwerftv1.TrafficPeer{{App: "xp-a/client"}},
		To:   []kwerftv1.TrafficPeer{{App: "server"}},
	})
	waitForRule(t, recv, "Applied")
	waitForRule(t, send, "Applied")

	// B withdraws; A's rule waits again. Then B drops isolation instead.
	if err := k8s.Delete(ctx, recv); err != nil {
		t.Fatal(err)
	}
	waitForRule(t, send, "AwaitingPeer")
	setIsolated(t, "xp-b", false)
	waitForRule(t, send, "Applied")

	// Not isolated: B's apps accept every project's pods.
	eventually(t, func() error {
		spec := ciliumSpec(t, "xp-b", "server")
		raw, _ := json.Marshal(spec)
		if !strings.Contains(string(raw), "namespace.labels.kwerft.dev/project") {
			return fmt.Errorf("server's policy does not admit other projects yet: %s", raw)
		}
		return nil
	})
}

// fakeCounter stands in for Hubble.
type fakeCounter struct {
	mu     sync.Mutex
	counts map[string]*kwerftv1.TrafficCounts
}

func (f *fakeCounter) RuleCounts(r *kwerftv1.TrafficRule) *kwerftv1.TrafficCounts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[r.Namespace+"/"+r.Name]
}

func TestTrafficRuleStatusCounts(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "tr-counts")
	since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	counts.mu.Lock()
	counts.counts = map[string]*kwerftv1.TrafficCounts{"tr-counts/public": {Allowed: 42, Dropped: 3, Since: &since}}
	counts.mu.Unlock()
	r := createRule(t, "tr-counts", "public", kwerftv1.TrafficRuleSpec{
		From: []kwerftv1.TrafficPeer{{Internet: true}},
		To:   []kwerftv1.TrafficPeer{{Project: "tr-counts"}},
	})
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(r), r); err != nil {
			return err
		}
		if c := r.Status.Counts; c == nil || c.Allowed != 42 || c.Dropped != 3 {
			return fmt.Errorf("counts = %+v", c)
		}
		return nil
	})
}

// counts is the TrafficRule reconciler's Hubble in these tests.
var counts = &fakeCounter{}
