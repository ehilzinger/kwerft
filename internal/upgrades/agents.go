// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Agent clusters (docs/phase6-upgrades.md › Agent clusters). Console N
// works with agents of the same minor and the one before (tunnel, CRDs,
// API). So the console upgrades first, then its agents, never past it; and
// a console upgrade that would leave a connected agent two minors behind
// is refused (the Kwerft preflight's AgentSkew check).

// AgentTargetAllowed says whether an agent running `agent` may be upgraded
// to `target` while the console runs `console`: newer than the agent, and
// not newer than the console.
func AgentTargetAllowed(console, agent, target string) error {
	t, err := ParseVersion(target)
	if err != nil {
		return err
	}
	c, err := ParseVersion(console)
	if err != nil {
		return fmt.Errorf("the console's version %q is not a release", console)
	}
	if c.Less(t) {
		return fmt.Errorf("the console runs %s: upgrade it to %s first, then its agents", console, target)
	}
	if a, err := ParseVersion(agent); err == nil && !a.Less(t) {
		return fmt.Errorf("the agent already runs %s", agent)
	}
	return nil
}

// AgentCompatible: console and agent are at most one minor apart, the
// agent not ahead.
func AgentCompatible(console, agent string) bool {
	c, err1 := ParseVersion(console)
	a, err2 := ParseVersion(agent)
	if err1 != nil || err2 != nil {
		return false
	}
	ahead := c.MinorsAhead(a)
	return ahead == 0 || ahead == 1
}

// AgentCluster is an agent cluster as the console's Cluster object
// describes it.
type AgentCluster struct {
	Name         string
	Connected    bool
	AgentVersion string
}

// FleetSkip is an agent cluster an "Upgrade all" leaves out, and why.
// Warning: it should be upgraded but cannot be now (disconnected, unknown
// version); otherwise it needs nothing.
type FleetSkip struct {
	Cluster string
	Reason  string
	Warning bool
}

// PlanFleet is which agent clusters an "Upgrade all" to target upgrades,
// in the order they go (by name), and which it skips.
func PlanFleet(target string, agents []AgentCluster) (queue []AgentCluster, skipped []FleetSkip) {
	t, terr := ParseVersion(target)
	sorted := slices.Clone(agents)
	slices.SortFunc(sorted, func(a, b AgentCluster) int { return strings.Compare(a.Name, b.Name) })
	for _, a := range sorted {
		v, err := ParseVersion(a.AgentVersion)
		switch {
		case !a.Connected:
			skipped = append(skipped, FleetSkip{Cluster: a.Name, Warning: true,
				Reason: "not connected: upgrade it when it is back, or re-run the installer there"})
		case err != nil || terr != nil:
			skipped = append(skipped, FleetSkip{Cluster: a.Name, Warning: true, Reason: fmt.Sprintf("its agent version %q is unknown", a.AgentVersion)})
		case !v.Less(t):
			skipped = append(skipped, FleetSkip{Cluster: a.Name, Reason: "already runs " + a.AgentVersion})
		default:
			queue = append(queue, a)
		}
	}
	return queue, skipped
}

// NewFleetID names a fleet that does not wait for a console upgrade (the
// console already runs the target). A fleet after the console's own
// upgrade uses that Upgrade's name.
func NewFleetID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "fleet-" + hex.EncodeToString(b)
}
