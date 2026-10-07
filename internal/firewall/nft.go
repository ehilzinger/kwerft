// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// The table and chains the installer creates (install.sh stage_firewall).
const (
	Table      = "inet kwerft"
	ChainSSH   = "managed_ssh"
	ChainOpen  = "managed_open"
	markerText = "kwerft managed "
)

// Marker is the comment Render puts in both managed chains, so the agent can
// tell whether the kernel still holds what it applied (an installer re-run
// or `systemctl restart nftables` empties them).
func Marker(hash string) string { return markerText + hash }

// Render returns the nft script that replaces both managed chains with
// rules, in one transaction (`nft -f -` applies all of it or nothing). It
// validates everything again: the ConfigMap is only data, and nothing but
// checked addresses, port numbers and rule names ever reaches nft.
func Render(rules NodeRules) (string, error) {
	var b strings.Builder
	hash := rules.Hash()
	fmt.Fprintf(&b, "flush chain %s %s\n", Table, ChainSSH)
	fmt.Fprintf(&b, "flush chain %s %s\n", Table, ChainOpen)
	fmt.Fprintf(&b, "add rule %s %s counter comment %q\n", Table, ChainSSH, Marker(hash))
	if len(rules.SSHSources) > 0 {
		ps, err := ParseSources(rules.SSHSources)
		if err != nil {
			return "", fmt.Errorf("ssh sources: %s", err.Message)
		}
		if len(ps) == 0 {
			return "", fmt.Errorf("ssh sources: none valid")
		}
		v4, v6 := split(ps)
		if len(v4) > 0 {
			fmt.Fprintf(&b, "add rule %s %s ip saddr %s accept\n", Table, ChainSSH, set(v4))
		}
		if len(v6) > 0 {
			fmt.Fprintf(&b, "add rule %s %s ip6 saddr %s accept\n", Table, ChainSSH, set(v6))
		}
		// Only public SSH jumps here (the base chain accepted established
		// connections, the private network and pods before).
		fmt.Fprintf(&b, "add rule %s %s drop\n", Table, ChainSSH)
	}
	fmt.Fprintf(&b, "add rule %s %s counter comment %q\n", Table, ChainOpen, Marker(hash))
	if len(rules.Open) > MaxOpen {
		return "", fmt.Errorf("more than %d open rules", MaxOpen)
	}
	for _, o := range rules.Open {
		if !ValidName(o.Rule) {
			return "", fmt.Errorf("rule name %q is not a DNS label", o.Rule)
		}
		if o.Protocol != "tcp" && o.Protocol != "udp" {
			return "", fmt.Errorf("rule %s: protocol %q", o.Rule, o.Protocol)
		}
		if o.Port < 1 || o.Port > 65535 || (o.EndPort != 0 && (o.EndPort < o.Port || o.EndPort > 65535)) {
			return "", fmt.Errorf("rule %s: port %d-%d", o.Rule, o.Port, o.EndPort)
		}
		for _, p := range requiredPorts {
			if strings.EqualFold(o.Protocol, p.proto) && o.Port <= p.port && p.port <= max(o.EndPort, o.Port) {
				return "", fmt.Errorf("rule %s: includes %s port %d", o.Rule, p.proto, p.port)
			}
		}
		ports := fmt.Sprint(o.Port)
		if o.EndPort > o.Port {
			ports = fmt.Sprintf("%d-%d", o.Port, o.EndPort)
		}
		match := fmt.Sprintf("%s dport %s accept comment %q", o.Protocol, ports, "rule "+o.Rule)
		if len(o.Sources) == 0 {
			fmt.Fprintf(&b, "add rule %s %s %s\n", Table, ChainOpen, match)
			continue
		}
		ps, err := ParseSources(o.Sources)
		if err != nil {
			return "", fmt.Errorf("rule %s: %s", o.Rule, err.Message)
		}
		if len(ps) == 0 {
			return "", fmt.Errorf("rule %s: no valid sources", o.Rule)
		}
		v4, v6 := split(ps)
		if len(v4) > 0 {
			fmt.Fprintf(&b, "add rule %s %s ip saddr %s %s\n", Table, ChainOpen, set(v4), match)
		}
		if len(v6) > 0 {
			fmt.Fprintf(&b, "add rule %s %s ip6 saddr %s %s\n", Table, ChainOpen, set(v6), match)
		}
	}
	return b.String(), nil
}

// split separates IPv4 and IPv6 prefixes and drops those inside another
// (nft refuses overlapping intervals in a set).
func split(ps []netip.Prefix) (v4, v6 []netip.Prefix) {
	for _, p := range collapse(ps) {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	return v4, v6
}

func collapse(ps []netip.Prefix) []netip.Prefix {
	sorted := slices.Clone(ps)
	slices.SortFunc(sorted, func(a, b netip.Prefix) int { return a.Bits() - b.Bits() })
	var out []netip.Prefix
	for _, p := range sorted {
		if !slices.ContainsFunc(out, func(o netip.Prefix) bool { return o.Addr().Is4() == p.Addr().Is4() && o.Contains(p.Addr()) }) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, comparePrefix)
	return out
}

func set(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return "{ " + strings.Join(s, ", ") + " }"
}

// HostState is what `nft list table inet kwerft` says about the host.
type HostState struct {
	// Prepared: the table has both managed chains and the base chain jumps
	// to them (an installer from Phase 4 on).
	Prepared bool
	// Problem explains a host that is not prepared.
	Problem string
	// Holds reports whether both managed chains carry the marker of hash.
	Holds func(hash string) bool
}

// ParseHost reads the output of `nft list table inet kwerft`.
func ParseHost(listing string) HostState {
	chains := map[string]string{}
	var current string
	for _, line := range strings.Split(listing, "\n") {
		t := strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(t, "chain "); ok {
			current = strings.TrimSuffix(strings.TrimSpace(name), "{")
			current = strings.TrimSpace(current)
			chains[current] = ""
			continue
		}
		if current != "" {
			chains[current] += t + "\n"
		}
	}
	hs := HostState{Holds: func(hash string) bool {
		m := fmt.Sprintf("comment %q", Marker(hash))
		return strings.Contains(chains[ChainSSH], m) && strings.Contains(chains[ChainOpen], m)
	}}
	input, ok := chains["input"]
	switch {
	case !ok:
		hs.Problem = "The installer's firewall table (inet kwerft) is missing on this node. Re-run the installer."
	case !hasChain(chains, ChainSSH) || !hasChain(chains, ChainOpen) ||
		!strings.Contains(input, "jump "+ChainSSH) || !strings.Contains(input, "jump "+ChainOpen):
		hs.Problem = "This node's firewall table is from an older installer. Re-run the installer on it to let Kwerft manage firewall rules."
	default:
		hs.Prepared = true
	}
	return hs
}

func hasChain(chains map[string]string, name string) bool {
	_, ok := chains[name]
	return ok
}
