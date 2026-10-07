// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package firewall turns FirewallRules into host rules on every node, safely.
//
// The installer's nftables table "inet kwerft" is the safety net: its base
// chain accepts established traffic, loopback, ICMP, pods, the private
// network, WireGuard and SSH/HTTP/HTTPS, and drops the rest. Kwerft only ever
// changes two empty chains the base chain jumps to:
//
//	managed_ssh   jumped for public SSH (tcp/22) before the base accept:
//	              empty, or "these sources accept, everything else drop"
//	managed_open  jumped after the base accepts, before the policy drop:
//	              the console's extra open ports
//
// So Kwerft can open ports and narrow SSH, but never touches HTTP(S), the
// cluster's own traffic, or SSH from the private network, and a flushed (or
// missing) managed chain always means "the installer's baseline".
//
// The controller (internal/controllers/firewall_controller.go) renders a
// Desired document per cluster into the ConfigMap kwerft-firewall; a node
// agent on every node (agent.go, `kwerft node-agent`) applies its node's part
// with `nft -f` and reports into kwerft-firewall-status. A change that takes
// anything away (a closed port, narrower SSH) is pending until an owner or
// admin confirms it in the console; without that within ConfirmWindow the
// agent restores the last confirmed rules by itself.
package firewall

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// Namespace holds the desired and status ConfigMaps (the chart's).
	Namespace = "kwerft-system"
	// DesiredConfigMap is written by the controller, read by the agents.
	DesiredConfigMap = "kwerft-firewall"
	// DesiredKey is the Desired document in DesiredConfigMap.
	DesiredKey = "desired.json"
	// SnapshotKey holds the FirewallRules as last confirmed, for "Roll back".
	SnapshotKey = "confirmed-rules.json"
	// StatusConfigMap has one key per node, written by that node's agent.
	StatusConfigMap = "kwerft-firewall-status"

	// LabelRequired marks the installer's base rules; they cannot be
	// deleted, and only the SSH rule's sources can change.
	LabelRequired = "kwerft.dev/required"

	// AnnotationConfirmed, on the required rule "ssh", is the revision an
	// owner or admin confirmed (the console writes it as that user).
	AnnotationConfirmed = "kwerft.dev/firewall-confirmed"
	// AnnotationAttempt, on "ssh", is bumped by "Apply again" after a
	// rollback: it changes the revision, so the agents try once more.
	AnnotationAttempt = "kwerft.dev/firewall-attempt"

	// ConfirmWindow is how long a pending change waits for confirmation.
	ConfirmWindow = 60 * time.Second

	// PodCIDR and WireGuardPort match POD_CIDR and WG_PORT in install.sh.
	PodCIDR       = "10.42.0.0/16"
	WireGuardPort = 51871
)

// Names of the required rules.
const (
	RuleSSH       = "ssh"
	RuleHTTP      = "http"
	RuleHTTPS     = "https"
	RuleWireGuard = "wireguard"
	RuleICMP      = "icmp"
	RuleCluster   = "cluster-private"
)

// Limits the agent enforces as well, so a hand-written ConfigMap cannot ask
// for more.
const (
	MaxSources = 64
	MaxOpen    = 200
)

// Required returns the installer's base rules as FirewallRules, for the
// console's full picture. privateNetwork is the cluster's private network
// (Cloud Network or vSwitch), empty without one. Every rule's spec is fixed
// except the SSH rule's sources.
func Required(privateNetwork string) []kwerftv1.FirewallRule {
	clusterSources := []string{PodCIDR}
	if privateNetwork != "" {
		clusterSources = append(clusterSources, privateNetwork)
	}
	mk := func(name string, spec kwerftv1.FirewallRuleSpec) kwerftv1.FirewallRule {
		r := kwerftv1.FirewallRule{Spec: spec}
		r.Name = name
		r.Labels = map[string]string{LabelRequired: "true", "app.kubernetes.io/managed-by": "kwerft"}
		if r.Spec.Nodes == "" {
			r.Spec.Nodes = "all"
		}
		return r
	}
	return []kwerftv1.FirewallRule{
		mk(RuleSSH, kwerftv1.FirewallRuleSpec{Port: 22, Protocol: "TCP",
			Description: "SSH"}),
		mk(RuleHTTP, kwerftv1.FirewallRuleSpec{Port: 80, Protocol: "TCP",
			Description: "HTTP to the ingress (redirects, Let's Encrypt)"}),
		mk(RuleHTTPS, kwerftv1.FirewallRuleSpec{Port: 443, Protocol: "TCP",
			Description: "HTTPS to the ingress and the console"}),
		mk(RuleWireGuard, kwerftv1.FirewallRuleSpec{Port: WireGuardPort, Protocol: "UDP",
			Description: "WireGuard between nodes (Cilium)"}),
		// ICMP has no port; the type requires one, so it carries 1.
		mk(RuleICMP, kwerftv1.FirewallRuleSpec{Port: 1, Protocol: "ICMP",
			Description: "Ping and path MTU (ICMP, ICMPv6)"}),
		mk(RuleCluster, kwerftv1.FirewallRuleSpec{Port: 1, EndPort: 65535, Protocol: "TCP", Sources: clusterSources,
			Description: "Cluster traffic: pods and the private network, every port"}),
	}
}

// IsRequired reports whether r is one of the installer's base rules.
func IsRequired(r *kwerftv1.FirewallRule) bool { return r.Labels[LabelRequired] == "true" }

// RequiredSpec returns the spec a required rule must have; for "ssh" that
// keeps current's sources. ok is false for a name that is not required.
func RequiredSpec(name, privateNetwork string, current kwerftv1.FirewallRuleSpec) (kwerftv1.FirewallRuleSpec, bool) {
	for _, r := range Required(privateNetwork) {
		if r.Name == name {
			spec := r.Spec
			if name == RuleSSH {
				spec.Sources = current.Sources
			}
			return spec, true
		}
	}
	return kwerftv1.FirewallRuleSpec{}, false
}

// IsRequiredName reports whether name is reserved for a required rule.
func IsRequiredName(name string) bool {
	switch name {
	case RuleSSH, RuleHTTP, RuleHTTPS, RuleWireGuard, RuleICMP, RuleCluster:
		return true
	}
	return false
}

// FieldError is a validation problem with the field it is about.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

func fieldErr(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// ParseSources parses CIDRs and plain addresses (a /32 or /128), refusing
// host bits ("10.0.1.3/16"), and returns them normalised, without
// duplicates, sorted.
func ParseSources(in []string) ([]netip.Prefix, *FieldError) {
	if len(in) > MaxSources {
		return nil, fieldErr("sources", "At most %d sources.", MaxSources)
	}
	var out []netip.Prefix
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(s, "/") {
			var err error
			p, err = netip.ParsePrefix(s)
			if err != nil {
				return nil, fieldErr("sources", "%q is not an IP address or a CIDR like 203.0.113.0/24.", s)
			}
			if p.Addr().Zone() != "" {
				return nil, fieldErr("sources", "%q has a zone; leave it out.", s)
			}
			if p.Addr().Is4In6() {
				p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
			}
			if m := p.Masked(); m != p {
				return nil, fieldErr("sources", "%s has host bits set; did you mean %s?", s, m)
			}
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil || a.Zone() != "" {
				return nil, fieldErr("sources", "%q is not an IP address or a CIDR like 203.0.113.0/24.", s)
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, comparePrefix)
	return out, nil
}

func comparePrefix(a, b netip.Prefix) int {
	return cmp.Or(a.Addr().Compare(b.Addr()), cmp.Compare(a.Bits(), b.Bits()))
}

// NormalizeSources is ParseSources as strings.
func NormalizeSources(in []string) ([]string, *FieldError) {
	ps, err := ParseSources(in)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out, nil
}

// Ports the required rules own: a custom rule may not include them, since
// the base chain decides them first (and SSH may be narrowed).
var requiredPorts = []struct {
	proto string
	port  int32
	rule  string
}{
	{"TCP", 22, "the required SSH rule; narrow its sources instead"},
	{"TCP", 80, "the required HTTP rule (open to everyone)"},
	{"TCP", 443, "the required HTTPS rule (open to everyone)"},
	{"UDP", WireGuardPort, "the required WireGuard rule (open to everyone)"},
}

// Cluster ports that must never be open to the whole internet; a rule for
// them needs sources.
var sensitivePorts = []struct {
	port int32
	what string
}{
	{2379, "etcd"}, {2380, "etcd peers"}, {6443, "the Kubernetes API"}, {10250, "the kubelet"},
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidName reports whether name is a valid rule name (a DNS label).
func ValidName(name string) bool { return len(name) <= 63 && nameRE.MatchString(name) }

// Validate checks a rule as the console and the controller accept it. A
// required rule must match its fixed spec (the controller resets it);
// only the SSH rule's sources are checked as sources.
func Validate(r *kwerftv1.FirewallRule) *FieldError {
	s := r.Spec
	if IsRequired(r) {
		if r.Name == RuleSSH {
			if _, err := ParseSources(s.Sources); err != nil {
				return err
			}
		}
		return nil
	}
	switch s.Protocol {
	case "TCP", "UDP":
	case "ICMP":
		return fieldErr("protocol", "ICMP is always allowed (the required rule icmp); open a TCP or UDP port.")
	default:
		return fieldErr("protocol", "Choose TCP or UDP.")
	}
	if s.Port < 1 || s.Port > 65535 {
		return fieldErr("port", "Enter a port from 1 to 65535.")
	}
	end := s.Port
	if s.EndPort != 0 {
		if s.EndPort < s.Port || s.EndPort > 65535 {
			return fieldErr("endPort", "The range must end at or after %d, at most 65535.", s.Port)
		}
		end = s.EndPort
	}
	for _, p := range requiredPorts {
		if s.Protocol == p.proto && s.Port <= p.port && p.port <= end {
			return fieldErr("port", "%s %d belongs to %s.", p.proto, p.port, p.rule)
		}
	}
	sources, err := ParseSources(s.Sources)
	if err != nil {
		return err
	}
	if anywhere(sources) && s.Protocol == "TCP" {
		for _, p := range sensitivePorts {
			if s.Port <= p.port && p.port <= end {
				return fieldErr("sources", "Port %d is %s; it must not be open to everyone. Add the addresses that may connect.", p.port, p.what)
			}
		}
	}
	switch s.Nodes {
	case "", "all", "control-plane":
	default:
		return fieldErr("nodes", "Choose all nodes or control-plane nodes.")
	}
	return nil
}

// anywhere: no sources, or one covering every address of a family.
func anywhere(ps []netip.Prefix) bool {
	if len(ps) == 0 {
		return true
	}
	for _, p := range ps {
		if p.Bits() == 0 {
			return true
		}
	}
	return false
}

// ---- the document the controller writes and the agents apply ------------------

// Open is one extra open port (range) on a node.
type Open struct {
	// Rule is the FirewallRule's name (a DNS label), kept as an nft comment.
	Rule     string `json:"rule"`
	Protocol string `json:"protocol"` // tcp | udp
	Port     int32  `json:"port"`
	EndPort  int32  `json:"endPort,omitempty"`
	// Sources are normalised prefixes; empty means anywhere.
	Sources []string `json:"sources,omitempty"`
}

// NodeRules is what one node's managed chains hold.
type NodeRules struct {
	// SSHSources narrows public SSH to these prefixes; empty leaves SSH as
	// the installer opened it.
	SSHSources []string `json:"sshSources,omitempty"`
	Open       []Open   `json:"open,omitempty"`
}

// Hash identifies the rules' content (the nft marker and state comparisons).
func (n NodeRules) Hash() string {
	raw, _ := json.Marshal(n)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// Empty reports whether the rules leave the installer's baseline alone.
func (n NodeRules) Empty() bool { return len(n.SSHSources) == 0 && len(n.Open) == 0 }

// Desired is the ConfigMap document: the cluster's revision and each node's
// rules.
type Desired struct {
	// Revision identifies the cluster's rule set (not which nodes exist, so
	// a node joining changes nothing for the others).
	Revision string `json:"revision"`
	// Confirmed is the newest revision confirmed in the console (or that
	// took nothing away anywhere). Nodes that missed it apply it directly.
	Confirmed string `json:"confirmed,omitempty"`
	// Nodes by name.
	Nodes map[string]NodeRules `json:"nodes"`
	// ControlPlane lists the nodes that are control-plane nodes.
	ControlPlane []string `json:"controlPlane,omitempty"`
}

// Node is what Build needs to know about a node.
type Node struct {
	Name         string
	ControlPlane bool
}

// Build renders the valid rules into a Desired document (without
// Confirmed). Invalid rules are returned by name and left out. attempt is
// AnnotationAttempt's value.
func Build(rules []kwerftv1.FirewallRule, nodes []Node, attempt string) (Desired, map[string]*FieldError) {
	invalid := map[string]*FieldError{}
	var ssh []string
	type scoped struct {
		Open
		ControlPlaneOnly bool `json:"cp,omitempty"`
	}
	var opens []scoped
	sorted := slices.Clone(rules)
	slices.SortFunc(sorted, func(a, b kwerftv1.FirewallRule) int { return strings.Compare(a.Name, b.Name) })
	for i := range sorted {
		r := &sorted[i]
		if !r.DeletionTimestamp.IsZero() {
			continue
		}
		if err := Validate(r); err != nil {
			invalid[r.Name] = err
			continue
		}
		if IsRequired(r) {
			if r.Name == RuleSSH {
				ssh, _ = NormalizeSources(r.Spec.Sources)
			}
			continue
		}
		if r.Spec.Disabled {
			continue
		}
		srcs, _ := ParseSources(r.Spec.Sources)
		o := Open{Rule: r.Name, Protocol: strings.ToLower(r.Spec.Protocol), Port: r.Spec.Port, EndPort: r.Spec.EndPort}
		if o.EndPort == o.Port {
			o.EndPort = 0
		}
		if !anywhere(srcs) {
			for _, p := range srcs {
				o.Sources = append(o.Sources, p.String())
			}
		}
		opens = append(opens, scoped{Open: o, ControlPlaneOnly: r.Spec.Nodes == "control-plane"})
		if len(opens) > MaxOpen {
			invalid[r.Name] = fieldErr("name", "At most %d open rules per cluster.", MaxOpen)
			opens = opens[:MaxOpen]
		}
	}
	raw, _ := json.Marshal(struct {
		SSH     []string `json:"ssh"`
		Opens   []scoped `json:"opens"`
		Attempt string   `json:"attempt,omitempty"`
	}{ssh, opens, attempt})
	sum := sha256.Sum256(raw)
	d := Desired{Revision: hex.EncodeToString(sum[:6]), Nodes: map[string]NodeRules{}}
	for _, n := range nodes {
		nr := NodeRules{SSHSources: ssh}
		for _, o := range opens {
			if !o.ControlPlaneOnly || n.ControlPlane {
				nr.Open = append(nr.Open, o.Open)
			}
		}
		d.Nodes[n.Name] = nr
		if n.ControlPlane {
			d.ControlPlane = append(d.ControlPlane, n.Name)
		}
	}
	slices.Sort(d.ControlPlane)
	return d, invalid
}

// ---- reachability ----------------------------------------------------------------

// SSHReaches reports whether addr reaches SSH with the given narrowing
// (empty: anywhere). always are networks the base chain accepts before
// SSH narrowing (the private network).
func SSHReaches(sources []string, addr netip.Addr, always ...netip.Prefix) bool {
	addr = addr.Unmap()
	ps, err := ParseSources(sources)
	if err != nil {
		return false
	}
	if len(ps) == 0 {
		return true
	}
	for _, p := range append(ps, always...) {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Verifiable reports whether addr is a client address a lock-out check can
// trust: not loopback, unspecified, link-local or inside the pod network
// (which means the request came through a proxy that hid the client).
func Verifiable(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() {
		return false
	}
	return !netip.MustParsePrefix(PodCIDR).Contains(addr)
}

// Permits reports whether next accepts at least everything prev accepts on
// a node: SSH from every source prev allowed, and every port prev opened to
// every source it opened it to. Such a change cannot lock anyone out, so the
// agent applies it without waiting for confirmation.
func Permits(prev, next NodeRules) bool {
	if len(next.SSHSources) > 0 {
		if len(prev.SSHSources) == 0 || !covers(next.SSHSources, prev.SSHSources) {
			return false
		}
	}
	for _, o := range prev.Open {
		if !slices.ContainsFunc(next.Open, func(n Open) bool { return opens(n, o) }) {
			return false
		}
	}
	return true
}

// opens: n accepts everything o does.
func opens(n, o Open) bool {
	if n.Protocol != o.Protocol {
		return false
	}
	oEnd, nEnd := cmp.Or(o.EndPort, o.Port), cmp.Or(n.EndPort, n.Port)
	if n.Port > o.Port || nEnd < oEnd {
		return false
	}
	if len(n.Sources) == 0 {
		return true
	}
	return len(o.Sources) > 0 && covers(n.Sources, o.Sources)
}

// covers: every prefix of inner lies inside a prefix of outer.
func covers(outer, inner []string) bool {
	out, err1 := ParseSources(outer)
	in, err2 := ParseSources(inner)
	if err1 != nil || err2 != nil {
		return false
	}
	for _, p := range in {
		if !slices.ContainsFunc(out, func(o netip.Prefix) bool {
			return o.Addr().Is4() == p.Addr().Is4() && o.Bits() <= p.Bits() && o.Contains(p.Addr())
		}) {
			return false
		}
	}
	return true
}
