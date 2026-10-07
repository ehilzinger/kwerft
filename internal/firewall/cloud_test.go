// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"fmt"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

func cloudCustom(name string, spec kwerftv1.FirewallRuleSpec) kwerftv1.FirewallRule {
	return kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

func TestCloudRulesBaselineAlwaysThere(t *testing.T) {
	// Even an empty set (the required rules missing) keeps the baseline.
	rules, states, err := CloudRules(nil)
	if err != nil || len(states) != 0 || !CloudKeepsBaseline(rules) || len(rules) != 5 {
		t.Fatalf("rules %+v, states %v, %v", rules, states, err)
	}
}

func TestCloudRules(t *testing.T) {
	set := Required("10.0.0.0/16")
	set[0].Spec.Sources = []string{"192.0.2.10", "2001:db8::/48"} // ssh
	set = append(set,
		cloudCustom("range", kwerftv1.FirewallRuleSpec{Port: 30000, EndPort: 30010, Protocol: "UDP"}),
		cloudCustom("same-port-twice", kwerftv1.FirewallRuleSpec{Port: 30000, EndPort: 30010, Protocol: "UDP", Sources: []string{"0.0.0.0/0"}}),
		cloudCustom("off", kwerftv1.FirewallRuleSpec{Port: 8080, Protocol: "TCP", Disabled: true}),
		cloudCustom("bad", kwerftv1.FirewallRuleSpec{Port: 443, Protocol: "TCP"}),
		cloudCustom("api", kwerftv1.FirewallRuleSpec{Port: 6443, Protocol: "TCP", Sources: []string{"198.51.100.0/24"}, Nodes: "control-plane"}),
	)
	rules, states, err := CloudRules(set)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ssh": CloudApplied, "http": CloudApplied, "cluster-private": CloudPrivate, "range": CloudApplied,
		"same-port-twice": CloudApplied, "off": CloudDisabled, "bad": CloudInvalid, "api": CloudApplied}
	for name, w := range want {
		if states[name] != w {
			t.Errorf("%s: %q, want %q", name, states[name], w)
		}
	}
	find := func(proto, port string) []string {
		for _, r := range rules {
			if r.Protocol == proto && r.Port == port {
				return r.SourceIPs
			}
		}
		return nil
	}
	if got := find("tcp", "22"); !slices.Equal(got, []string{"192.0.2.10/32", "2001:db8::/48"}) {
		t.Errorf("ssh sources %v", got)
	}
	if got := find("udp", "30000-30010"); !slices.Equal(got, Anywhere) {
		t.Errorf("range %v", got)
	}
	if got := find("tcp", "6443"); !slices.Equal(got, []string{"198.51.100.0/24"}) {
		t.Errorf("api %v", got)
	}
	if find("tcp", "8080") != nil {
		t.Error("a disabled rule was rendered")
	}
	if len(rules) != 7 { // 5 baseline + range (deduplicated) + api
		t.Errorf("%d rules: %+v", len(rules), rules)
	}
	if !CloudKeepsBaseline(rules) {
		t.Error("baseline lost")
	}
}

func TestCloudRulesTooMany(t *testing.T) {
	var set []kwerftv1.FirewallRule
	for i := range hetzner.MaxFirewallRules {
		set = append(set, cloudCustom(fmt.Sprintf("r%02d", i), kwerftv1.FirewallRuleSpec{Port: int32(20000 + i), Protocol: "TCP"}))
	}
	if _, _, err := CloudRules(set); err == nil {
		t.Error("more than 50 Cloud rules accepted")
	}
}

func TestSameCloudRules(t *testing.T) {
	a := []hetzner.FirewallRule{{Direction: "in", Protocol: "tcp", Port: "22", SourceIPs: []string{"::/0", "0.0.0.0/0"}}, {Direction: "in", Protocol: "icmp", SourceIPs: Anywhere}}
	b := []hetzner.FirewallRule{{Direction: "in", Protocol: "icmp", SourceIPs: Anywhere}, {Direction: "in", Protocol: "tcp", Port: "22", SourceIPs: Anywhere}}
	if !SameCloudRules(a, b) {
		t.Error("order or source order should not matter")
	}
	b[1].Port = "2222"
	if SameCloudRules(a, b) {
		t.Error("different ports compared equal")
	}
	if CloudKeepsBaseline(a) {
		t.Error("baseline without HTTP(S) and WireGuard")
	}
}
