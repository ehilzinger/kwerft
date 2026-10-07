// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// The Hetzner Cloud Firewall in front of a cluster's Cloud servers carries
// the public, inbound part of the host firewall: what the base chain
// accepts from the internet (SSH, HTTP, HTTPS, WireGuard, ICMP) and the
// console's open ports. It is a second layer (defence in depth); the host
// firewall stays as it is. Cloud Firewalls do not filter private networks,
// so the rule cluster-private (pods and the private network) has no Cloud
// counterpart.
//
// The controller feeds CloudRules the rule set as last confirmed on the
// nodes (the snapshot "Roll back now" restores), never a pending change: a
// change that could lock someone out reaches the Cloud Firewall only after
// the console confirmed it on the hosts, and a rollback on the hosts never
// needs an API call.

// Cloud rule states, per FirewallRule (FirewallRuleStatus.cloudFirewall).
const (
	CloudApplied  = "Applied"
	CloudPending  = "Pending"
	CloudPrivate  = "Private"
	CloudDisabled = "Disabled"
	CloudInvalid  = "Invalid"
	CloudOff      = "Off"
)

// Anywhere is every IPv4 and IPv6 address, as Cloud Firewall sources.
var Anywhere = []string{"0.0.0.0/0", "::/0"}

// CloudRules renders rules into Cloud Firewall rules and says, per rule
// name, what became of it. The required public rules are always there, also
// when the set lacks them: SSH (narrowed only by the rule ssh's sources),
// HTTP, HTTPS, WireGuard and ICMP. An error means the set does not fit into
// one Cloud Firewall; the caller keeps the rules it has.
func CloudRules(rules []kwerftv1.FirewallRule) ([]hetzner.FirewallRule, map[string]string, error) {
	states := map[string]string{}
	sorted := slices.Clone(rules)
	slices.SortFunc(sorted, func(a, b kwerftv1.FirewallRule) int { return strings.Compare(a.Name, b.Name) })

	sshSources := Anywhere
	var opens []hetzner.FirewallRule
	for i := range sorted {
		r := &sorted[i]
		if !r.DeletionTimestamp.IsZero() {
			continue
		}
		if err := Validate(r); err != nil {
			states[r.Name] = CloudInvalid
			continue
		}
		if IsRequired(r) {
			switch r.Name {
			case RuleSSH:
				if srcs, _ := NormalizeSources(r.Spec.Sources); len(srcs) > 0 && !anywhereStrings(srcs) {
					sshSources = srcs
				}
				states[r.Name] = CloudApplied
			case RuleCluster:
				states[r.Name] = CloudPrivate
			default:
				states[r.Name] = CloudApplied
			}
			continue
		}
		if r.Spec.Disabled {
			states[r.Name] = CloudDisabled
			continue
		}
		srcs, _ := NormalizeSources(r.Spec.Sources)
		if len(srcs) == 0 || anywhereStrings(srcs) {
			srcs = Anywhere
		}
		port := strconv.Itoa(int(r.Spec.Port))
		if r.Spec.EndPort != 0 && r.Spec.EndPort != r.Spec.Port {
			port += "-" + strconv.Itoa(int(r.Spec.EndPort))
		}
		opens = append(opens, inRules(strings.ToLower(r.Spec.Protocol), port, srcs)...)
		states[r.Name] = CloudApplied
	}

	out := slices.Concat(
		inRules("tcp", "22", sshSources),
		inRules("tcp", "80", Anywhere),
		inRules("tcp", "443", Anywhere),
		inRules("udp", strconv.Itoa(WireGuardPort), Anywhere),
		inRules("icmp", "", Anywhere),
	)
	for _, o := range opens {
		if !slices.ContainsFunc(out, func(x hetzner.FirewallRule) bool { return SameCloudRule(x, o) }) {
			out = append(out, o)
		}
	}
	if len(out) > hetzner.MaxFirewallRules {
		return nil, states, fmt.Errorf("the rules need %d Cloud Firewall rules; a Cloud Firewall holds at most %d. Combine port ranges or remove rules", len(out), hetzner.MaxFirewallRules)
	}
	return out, states, nil
}

// inRules is one inbound rule, split when it has more sources than a Cloud
// Firewall rule takes.
func inRules(proto, port string, sources []string) []hetzner.FirewallRule {
	var out []hetzner.FirewallRule
	for chunk := range slices.Chunk(sources, hetzner.MaxFirewallRuleBlocks) {
		out = append(out, hetzner.FirewallRule{Direction: "in", Protocol: proto, Port: port, SourceIPs: slices.Clone(chunk)})
	}
	return out
}

func anywhereStrings(srcs []string) bool {
	ps, err := ParseSources(srcs)
	return err == nil && anywhere(ps)
}

// SameCloudRule compares two Cloud Firewall rules as the API stores them
// (sources as a set of normalised prefixes).
func SameCloudRule(a, b hetzner.FirewallRule) bool {
	if a.Direction != b.Direction || a.Protocol != b.Protocol || a.Port != b.Port {
		return false
	}
	return slices.Equal(normalizedSet(a.SourceIPs), normalizedSet(b.SourceIPs)) &&
		slices.Equal(normalizedSet(a.DestinationIPs), normalizedSet(b.DestinationIPs))
}

// SameCloudRules compares rule lists regardless of order.
func SameCloudRules(a, b []hetzner.FirewallRule) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		found := false
		for j, y := range b {
			if !used[j] && SameCloudRule(x, y) {
				used[j], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func normalizedSet(in []string) []string {
	ps, err := ParseSources(in)
	if err != nil {
		out := slices.Clone(in)
		slices.Sort(out)
		return out
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

// CloudSSHReaches reports whether the Cloud rules let addr reach SSH: the
// last check before rules are sent to Hetzner.
func CloudSSHReaches(rules []hetzner.FirewallRule) bool {
	for _, r := range rules {
		if r.Direction == "in" && r.Protocol == "tcp" && portCovers(r.Port, 22) && len(r.SourceIPs) > 0 {
			return true
		}
	}
	return false
}

// CloudKeepsBaseline reports whether rules keep HTTP, HTTPS and WireGuard
// open to everyone and SSH open to someone: the never-lock-out invariant.
func CloudKeepsBaseline(rules []hetzner.FirewallRule) bool {
	open := func(proto string, port int) bool {
		return slices.ContainsFunc(rules, func(r hetzner.FirewallRule) bool {
			srcs := normalizedSet(r.SourceIPs)
			return r.Direction == "in" && r.Protocol == proto && portCovers(r.Port, port) &&
				slices.Contains(srcs, "0.0.0.0/0") && slices.Contains(srcs, "::/0")
		})
	}
	return CloudSSHReaches(rules) && open("tcp", 80) && open("tcp", 443) && open("udp", WireGuardPort)
}

func portCovers(spec string, port int) bool {
	lo, hi, isRange := strings.Cut(spec, "-")
	a, err := strconv.Atoi(lo)
	if err != nil {
		return false
	}
	b := a
	if isRange {
		if b, err = strconv.Atoi(hi); err != nil {
			return false
		}
	}
	return a <= port && port <= b
}
