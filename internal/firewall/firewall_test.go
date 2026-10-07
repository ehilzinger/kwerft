// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"net/netip"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func custom(name string, spec kwerftv1.FirewallRuleSpec) kwerftv1.FirewallRule {
	return kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  kwerftv1.FirewallRuleSpec
		field string // "" = valid
		msg   string
	}{
		{"plain port", kwerftv1.FirewallRuleSpec{Port: 8080, Protocol: "TCP"}, "", ""},
		{"udp range from a network", kwerftv1.FirewallRuleSpec{Port: 60000, EndPort: 61000, Protocol: "UDP", Sources: []string{"203.0.113.0/24", "2001:db8::/32"}}, "", ""},
		{"a plain address", kwerftv1.FirewallRuleSpec{Port: 8080, Protocol: "TCP", Sources: []string{"203.0.113.7"}}, "", ""},
		{"icmp", kwerftv1.FirewallRuleSpec{Port: 1, Protocol: "ICMP"}, "protocol", "always allowed"},
		{"ssh", kwerftv1.FirewallRuleSpec{Port: 22, Protocol: "TCP"}, "port", "SSH"},
		{"range over https", kwerftv1.FirewallRuleSpec{Port: 400, EndPort: 500, Protocol: "TCP"}, "port", "HTTPS"},
		{"udp 443 is fine", kwerftv1.FirewallRuleSpec{Port: 443, Protocol: "UDP"}, "", ""},
		{"wireguard", kwerftv1.FirewallRuleSpec{Port: 51800, EndPort: 51900, Protocol: "UDP"}, "port", "WireGuard"},
		{"api to everyone", kwerftv1.FirewallRuleSpec{Port: 6443, Protocol: "TCP"}, "sources", "Kubernetes API"},
		{"api to everyone, spelled out", kwerftv1.FirewallRuleSpec{Port: 6443, Protocol: "TCP", Sources: []string{"0.0.0.0/0"}}, "sources", "Kubernetes API"},
		{"api to an office", kwerftv1.FirewallRuleSpec{Port: 6443, Protocol: "TCP", Sources: []string{"198.51.100.0/24"}}, "", ""},
		{"host bits", kwerftv1.FirewallRuleSpec{Port: 8080, Protocol: "TCP", Sources: []string{"10.0.1.3/16"}}, "sources", "10.0.0.0/16"},
		{"garbage source", kwerftv1.FirewallRuleSpec{Port: 8080, Protocol: "TCP", Sources: []string{"office"}}, "sources", "not an IP"},
		{"backwards range", kwerftv1.FirewallRuleSpec{Port: 9000, EndPort: 8000, Protocol: "TCP"}, "endPort", ""},
		{"bad nodes", kwerftv1.FirewallRuleSpec{Port: 8080, Protocol: "TCP", Nodes: "workers"}, "nodes", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := custom("x", tc.spec)
			err := Validate(&r)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || err.Field != tc.field || !strings.Contains(err.Message, tc.msg) {
				t.Fatalf("want %s error containing %q, got %v", tc.field, tc.msg, err)
			}
		})
	}
}

func TestParseSources(t *testing.T) {
	got, err := NormalizeSources([]string{" 203.0.113.7 ", "2001:db8::/32", "203.0.113.7/32", "::ffff:198.51.100.0/120", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := "198.51.100.0/24 203.0.113.7/32 2001:db8::/32"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestRequiredRulesValidateAndOnlySSHSourcesChange(t *testing.T) {
	for _, r := range Required("10.0.0.0/16") {
		if err := Validate(&r); err != nil {
			t.Errorf("%s: %v", r.Name, err)
		}
		if !IsRequired(&r) || !IsRequiredName(r.Name) {
			t.Errorf("%s: not marked required", r.Name)
		}
	}
	spec, ok := RequiredSpec(RuleSSH, "", kwerftv1.FirewallRuleSpec{Port: 2222, Protocol: "UDP", Sources: []string{"203.0.113.0/24"}})
	if !ok || spec.Port != 22 || spec.Protocol != "TCP" || len(spec.Sources) != 1 {
		t.Fatalf("ssh spec: %+v", spec)
	}
	spec, _ = RequiredSpec(RuleHTTPS, "", kwerftv1.FirewallRuleSpec{Port: 443, Protocol: "TCP", Sources: []string{"203.0.113.0/24"}})
	if len(spec.Sources) != 0 {
		t.Fatalf("https sources must stay open: %+v", spec)
	}
	cluster, _ := RequiredSpec(RuleCluster, "10.0.0.0/16", kwerftv1.FirewallRuleSpec{})
	if strings.Join(cluster.Sources, " ") != PodCIDR+" 10.0.0.0/16" {
		t.Fatalf("cluster sources: %v", cluster.Sources)
	}
}

func TestBuild(t *testing.T) {
	rules := Required("10.0.0.0/16")
	rules[0].Spec.Sources = []string{"203.0.113.7"} // ssh
	rules = append(rules,
		custom("api", kwerftv1.FirewallRuleSpec{Port: 6443, Protocol: "TCP", Sources: []string{"198.51.100.0/24"}, Nodes: "control-plane"}),
		custom("game", kwerftv1.FirewallRuleSpec{Port: 27015, EndPort: 27015, Protocol: "UDP"}),
		custom("off", kwerftv1.FirewallRuleSpec{Port: 9000, Protocol: "TCP", Disabled: true}),
		custom("broken", kwerftv1.FirewallRuleSpec{Port: 22, Protocol: "TCP"}),
	)
	nodes := []Node{{Name: "cp1", ControlPlane: true}, {Name: "w1"}}
	d, invalid := Build(rules, nodes, "")
	if len(invalid) != 1 || invalid["broken"] == nil {
		t.Fatalf("invalid: %v", invalid)
	}
	if got := d.Nodes["cp1"]; len(got.Open) != 2 || got.SSHSources[0] != "203.0.113.7/32" {
		t.Fatalf("cp1: %+v", got)
	}
	if got := d.Nodes["w1"]; len(got.Open) != 1 || got.Open[0].Rule != "game" || got.Open[0].EndPort != 0 {
		t.Fatalf("w1: %+v", got)
	}
	if len(d.ControlPlane) != 1 || d.ControlPlane[0] != "cp1" {
		t.Fatalf("control plane: %v", d.ControlPlane)
	}

	// The revision is the rule set's, not the node list's.
	d2, _ := Build(rules, append(nodes, Node{Name: "w2"}), "")
	if d2.Revision != d.Revision {
		t.Fatal("a joining node changed the revision")
	}
	d3, _ := Build(rules, nodes, "1")
	if d3.Revision == d.Revision {
		t.Fatal("a new attempt must change the revision")
	}
	rules[len(rules)-2].Spec.Disabled = false // "off"
	d4, _ := Build(rules, nodes, "")
	if d4.Revision == d.Revision {
		t.Fatal("enabling a rule must change the revision")
	}
}

func TestPermits(t *testing.T) {
	open := func(port int32, src ...string) Open {
		return Open{Rule: "r", Protocol: "tcp", Port: port, Sources: src}
	}
	for _, tc := range []struct {
		name       string
		prev, next NodeRules
		want       bool
	}{
		{"nothing to nothing", NodeRules{}, NodeRules{}, true},
		{"open a port", NodeRules{}, NodeRules{Open: []Open{open(8080)}}, true},
		{"close a port", NodeRules{Open: []Open{open(8080)}}, NodeRules{}, false},
		{"narrow a port", NodeRules{Open: []Open{open(8080)}}, NodeRules{Open: []Open{open(8080, "203.0.113.0/24")}}, false},
		{"widen a port", NodeRules{Open: []Open{open(8080, "203.0.113.0/24")}}, NodeRules{Open: []Open{open(8080, "203.0.0.0/16")}}, true},
		{"narrow ssh", NodeRules{}, NodeRules{SSHSources: []string{"203.0.113.0/24"}}, false},
		{"un-narrow ssh", NodeRules{SSHSources: []string{"203.0.113.0/24"}}, NodeRules{}, true},
		{"add an ssh source", NodeRules{SSHSources: []string{"203.0.113.0/24"}}, NodeRules{SSHSources: []string{"198.51.100.1/32", "203.0.113.0/24"}}, true},
		{"swap the ssh source", NodeRules{SSHSources: []string{"203.0.113.0/24"}}, NodeRules{SSHSources: []string{"198.51.100.0/24"}}, false},
		{"range covers a port", NodeRules{Open: []Open{open(8080)}}, NodeRules{Open: []Open{{Rule: "x", Protocol: "tcp", Port: 8000, EndPort: 9000}}}, true},
		{"other protocol", NodeRules{Open: []Open{open(8080)}}, NodeRules{Open: []Open{{Rule: "x", Protocol: "udp", Port: 8080}}}, false},
	} {
		if got := Permits(tc.prev, tc.next); got != tc.want {
			t.Errorf("%s: Permits = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSSHReaches(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.7")
	private := netip.MustParsePrefix("10.0.0.0/16")
	if !SSHReaches(nil, ip) {
		t.Fatal("no narrowing reaches everyone")
	}
	if !SSHReaches([]string{"203.0.113.0/24"}, ip) || SSHReaches([]string{"198.51.100.0/24"}, ip) {
		t.Fatal("sources")
	}
	if !SSHReaches([]string{"198.51.100.0/24"}, netip.MustParseAddr("10.0.0.5"), private) {
		t.Fatal("the private network always reaches SSH")
	}
	if !SSHReaches([]string{"203.0.113.7"}, netip.MustParseAddr("::ffff:203.0.113.7")) {
		t.Fatal("IPv4-mapped addresses")
	}
	for _, s := range []string{"127.0.0.1", "10.42.3.4", "::1", "fe80::1", "0.0.0.0"} {
		if Verifiable(netip.MustParseAddr(s)) {
			t.Errorf("%s must not count as a client's address", s)
		}
	}
	if !Verifiable(ip) || !Verifiable(netip.MustParseAddr("10.0.0.5")) {
		t.Fatal("public and private network addresses are verifiable")
	}
}

func TestRender(t *testing.T) {
	script, err := Render(NodeRules{
		SSHSources: []string{"203.0.113.0/24", "203.0.113.7/32", "2001:db8::/32"},
		Open: []Open{
			{Rule: "game", Protocol: "udp", Port: 27015, EndPort: 27030},
			{Rule: "api", Protocol: "tcp", Port: 6443, Sources: []string{"198.51.100.0/24"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"flush chain inet kwerft managed_ssh",
		"flush chain inet kwerft managed_open",
		`add rule inet kwerft managed_ssh counter comment "kwerft managed `,
		"add rule inet kwerft managed_ssh ip saddr { 203.0.113.0/24 } accept",
		"add rule inet kwerft managed_ssh ip6 saddr { 2001:db8::/32 } accept",
		"add rule inet kwerft managed_ssh drop",
		`add rule inet kwerft managed_open counter comment "kwerft managed `,
		`add rule inet kwerft managed_open udp dport 27015-27030 accept comment "rule game"`,
		`add rule inet kwerft managed_open ip saddr { 198.51.100.0/24 } tcp dport 6443 accept comment "rule api"`,
	}
	lines := strings.Split(strings.TrimSpace(script), "\n")
	if len(lines) != len(want) {
		t.Fatalf("script:\n%s", script)
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Errorf("line %d: %q, want %q", i, lines[i], want[i])
		}
	}

	empty, _ := Render(NodeRules{})
	if strings.Contains(empty, "drop") || strings.Count(empty, "\n") != 4 {
		t.Fatalf("empty rules must only flush and mark:\n%s", empty)
	}

	for _, bad := range []NodeRules{
		{Open: []Open{{Rule: `x" ; flush ruleset ; "`, Protocol: "tcp", Port: 1}}},
		{Open: []Open{{Rule: "x", Protocol: "icmp", Port: 1}}},
		{Open: []Open{{Rule: "x", Protocol: "tcp", Port: 0}}},
		{Open: []Open{{Rule: "x", Protocol: "tcp", Port: 1, EndPort: 1000}}}, // includes 22
		{Open: []Open{{Rule: "x", Protocol: "tcp", Port: 8080, Sources: []string{"0.0.0.0/0 } accept ; drop {"}}}},
		{SSHSources: []string{"not-an-ip"}},
	} {
		if s, err := Render(bad); err == nil {
			t.Errorf("rendered %+v:\n%s", bad, s)
		}
	}
}

func TestParseHost(t *testing.T) {
	if hs := ParseHost(""); hs.Prepared || !strings.Contains(hs.Problem, "missing") {
		t.Fatalf("no table: %+v", hs)
	}
	old := "table inet kwerft {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\ttcp dport { 22, 80, 443 } accept\n\t}\n}\n"
	if hs := ParseHost(old); hs.Prepared || !strings.Contains(hs.Problem, "older installer") {
		t.Fatalf("old table: %+v", hs)
	}
	f := newFakeNFT()
	listing, _ := f.Run(t.Context(), "", "list")
	hs := ParseHost(listing)
	if !hs.Prepared || hs.Holds(NodeRules{}.Hash()) {
		t.Fatalf("fresh table: %+v", hs)
	}
	script, _ := Render(NodeRules{})
	_, _ = f.Run(t.Context(), script, "-f", "-")
	listing, _ = f.Run(t.Context(), "", "list")
	if !ParseHost(listing).Holds(NodeRules{}.Hash()) {
		t.Fatalf("marker not found in:\n%s", listing)
	}
}
