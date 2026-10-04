package firewall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// NFT runs nft on the host: args, with stdin as the script for "-f -".
type NFT interface {
	Run(ctx context.Context, stdin string, args ...string) (string, error)
}

// Node states the agent reports.
const (
	StateInSync     = "in-sync"     // the confirmed rules are applied
	StatePending    = "pending"     // a change is applied and waits for confirmation
	StateRolledBack = "rolled-back" // the desired revision was rolled back; the confirmed rules are applied
	StateError      = "error"       // the desired rules could not be applied; the confirmed ones are
	StatePaused     = "paused"      // install.sh --reset-firewall: Kwerft's rules are off on this node
	StateUnprepared = "unprepared"  // the host has no (or an old) firewall table
)

// NodeStatus is one node's entry in the status ConfigMap.
type NodeStatus struct {
	State string `json:"state"`
	// Seen is the desired revision the agent last read.
	Seen string `json:"seen,omitempty"`
	// Confirmed is the revision whose rules are confirmed on this node.
	Confirmed string `json:"confirmed,omitempty"`
	// Pending, with Deadline, is the change waiting for confirmation.
	Pending  string     `json:"pending,omitempty"`
	Deadline *time.Time `json:"deadline,omitempty"`
	// RolledBack is the last revision the agent rolled back, and when.
	RolledBack   string     `json:"rolledBack,omitempty"`
	RolledBackAt *time.Time `json:"rolledBackAt,omitempty"`
	Message      string     `json:"message,omitempty"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	Version      string     `json:"version,omitempty"`
}

type ruleset struct {
	Revision string    `json:"revision"`
	Rules    NodeRules `json:"rules"`
}

type pending struct {
	ruleset
	AppliedAt time.Time `json:"appliedAt"`
}

// State is what the agent keeps on the host (survives restarts and reboots,
// so a pending change still rolls back on time).
type State struct {
	Confirmed    ruleset    `json:"confirmed"`
	Pending      *pending   `json:"pending,omitempty"`
	RolledBack   string     `json:"rolledBack,omitempty"`
	RolledBackAt *time.Time `json:"rolledBackAt,omitempty"`
	// Failed is a revision whose rules nft refused, with why; not retried.
	Failed      string `json:"failed,omitempty"`
	FailedError string `json:"failedError,omitempty"`
}

// Agent applies one node's rules. It is not safe for concurrent use: the
// node agent's single loop calls Step.
type Agent struct {
	Node string
	NFT  NFT
	// Dir keeps state.json; a file "paused" in it turns the agent off
	// (install.sh --reset-firewall writes it).
	Dir     string
	Now     func() time.Time
	Window  time.Duration // ConfirmWindow when zero
	Log     *slog.Logger
	Version string

	state    State
	loaded   bool
	applied  string // hash of what the agent last applied
	applyErr string // the confirmed rules could not be applied (retried every step)
	message  string
}

// PausedFile is the marker install.sh --reset-firewall leaves in Dir.
const PausedFile = "paused"

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) window() time.Duration {
	if a.Window > 0 {
		return a.Window
	}
	return ConfirmWindow
}

func (a *Agent) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Load reads the state left by an earlier run. A missing file means a new
// node: nothing confirmed beyond the installer's baseline.
func (a *Agent) Load() error {
	a.loaded = true
	raw, err := os.ReadFile(filepath.Join(a.Dir, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &a.state); err != nil {
		// A broken file must not keep the node on a pending change.
		a.log().Error("state file unreadable; starting from the baseline", "err", err)
		a.state = State{}
	}
	return nil
}

func (a *Agent) save() error {
	raw, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	tmp := filepath.Join(a.Dir, "state.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(a.Dir, "state.json"))
}

// State returns a copy of the agent's state (tests).
func (a *Agent) State() State { return a.state }

func (a *Agent) paused() bool {
	_, err := os.Stat(filepath.Join(a.Dir, PausedFile))
	return err == nil
}

// Step advances the state machine: it rolls back a pending change whose
// window ran out, takes in d (nil when the desired state could not be read:
// the agent then only keeps time and the kernel in order), and makes the
// kernel hold the rules in effect. It returns the status to report.
func (a *Agent) Step(ctx context.Context, d *Desired) NodeStatus {
	if !a.loaded {
		if err := a.Load(); err != nil {
			return a.status(StateError, "Cannot read the agent's state: "+err.Error(), "")
		}
	}
	now := a.now()
	seen := ""
	if d != nil {
		seen = d.Revision
	}

	// 1. Time first: a pending change rolls back on time even when the
	// console or the Kubernetes API are gone.
	if p := a.state.Pending; p != nil && !now.Before(p.AppliedAt.Add(a.window())) {
		a.rollBack(now, "not confirmed within "+a.window().String())
	}

	listing, err := a.NFT.Run(ctx, "", "list", "table", "inet", "kwerft")
	host := ParseHost(listing)
	if err != nil && host.Problem == "" {
		host.Problem = "nft failed: " + err.Error()
	}
	prepared := err == nil && host.Prepared
	if !prepared {
		a.applied = ""
	}

	if a.paused() {
		if prepared && !host.Holds(NodeRules{}.Hash()) {
			a.applied = ""
			if err := a.apply(ctx, NodeRules{}); err != nil {
				return a.status(StateError, "Paused, but clearing Kwerft's rules failed: "+err.Error(), seen)
			}
		}
		a.applied = ""
		return a.status(StatePaused, "Kwerft's firewall rules are off on this node (install.sh --reset-firewall). Delete "+
			filepath.Join("/var/lib/kwerft/firewall", PausedFile)+" on the node to turn them on again.", seen)
	}
	if !prepared {
		return a.status(StateUnprepared, host.Problem, seen)
	}

	// 2. Take in the desired rules.
	if d != nil {
		if rules, ok := d.Nodes[a.Node]; ok {
			a.take(d, rules, now)
		}
	}

	// 3. The kernel holds what is in effect (re-applied after an installer
	// run or an nftables restart emptied the chains).
	eff := a.effective()
	if !host.Holds(eff.Rules.Hash()) {
		a.applied = ""
	}
	a.applyErr = ""
	if err := a.apply(ctx, eff.Rules); err != nil {
		if a.state.Pending != nil {
			// nft refused the change and left the kernel as it was; drop
			// it, and put the confirmed rules back on the next step.
			a.log().Error("applying a firewall change failed; keeping the confirmed rules", "revision", eff.Revision, "err", err)
			a.state.Failed, a.state.FailedError, a.state.Pending = eff.Revision, "nft refused the rules: "+err.Error(), nil
			if err := a.save(); err != nil {
				a.log().Error("saving the agent's state failed", "err", err)
			}
		} else {
			a.applyErr = "Applying the confirmed rules failed: " + err.Error()
			a.log().Error("applying the confirmed firewall rules failed", "revision", eff.Revision, "err", err)
		}
	}

	switch {
	case a.applyErr != "":
		return a.status(StateError, a.applyErr, seen)
	case a.state.Pending != nil:
		return a.status(StatePending, "", seen)
	case a.state.Failed != "" && a.state.Failed == seen:
		return a.status(StateError, a.state.FailedError, seen)
	case a.state.RolledBack != "" && a.state.RolledBack == seen:
		return a.status(StateRolledBack, a.message, seen)
	}
	return a.status(StateInSync, "", seen)
}

// take decides what to do with the desired rules for this node.
func (a *Agent) take(d *Desired, rules NodeRules, now time.Time) {
	h := rules.Hash()
	st := &a.state
	defer func() {
		if err := a.save(); err != nil {
			a.log().Error("saving the agent's state failed", "err", err)
		}
	}()
	switch {
	case st.Pending != nil && st.Pending.Revision == d.Revision && d.Confirmed == d.Revision && st.Pending.Rules.Hash() == h:
		a.log().Info("change confirmed", "revision", d.Revision)
		st.Confirmed, st.Pending = st.Pending.ruleset, nil
	case h == st.Confirmed.Rules.Hash():
		// Nothing changes on this node; a pending change was discarded.
		if st.Pending != nil {
			a.log().Info("pending change discarded", "revision", st.Pending.Revision)
		}
		st.Pending = nil
		st.Confirmed.Revision = d.Revision
	case d.Revision == st.RolledBack || d.Revision == st.Failed:
		// Rolled back or refused: stay on the confirmed rules until the
		// rules change or someone asks to apply them again.
	case st.Pending != nil && st.Pending.Revision == d.Revision && st.Pending.Rules.Hash() == h:
		// Waiting for confirmation.
	case d.Confirmed == d.Revision || Permits(st.Confirmed.Rules, rules):
		// Confirmed in the console already (a node that missed it, or a
		// new one), or nothing is taken away: no confirmation needed.
		a.log().Info("applying confirmed rules", "revision", d.Revision)
		st.Confirmed, st.Pending = ruleset{Revision: d.Revision, Rules: rules}, nil
	default:
		if _, err := Render(rules); err != nil {
			st.Failed, st.FailedError = d.Revision, "The rules cannot be applied: "+err.Error()
			return
		}
		a.log().Info("applying a change; it rolls back unless confirmed", "revision", d.Revision, "window", a.window())
		st.Pending = &pending{ruleset: ruleset{Revision: d.Revision, Rules: rules}, AppliedAt: now}
	}
}

func (a *Agent) effective() ruleset {
	if a.state.Pending != nil {
		return a.state.Pending.ruleset
	}
	return a.state.Confirmed
}

func (a *Agent) rollBack(now time.Time, why string) {
	p := a.state.Pending
	a.log().Warn("rolling back a firewall change", "revision", p.Revision, "why", why)
	a.state.RolledBack, a.state.RolledBackAt, a.state.Pending = p.Revision, &now, nil
	a.message = "Rolled back: " + why + "."
	if err := a.save(); err != nil {
		a.log().Error("saving the agent's state failed", "err", err)
	}
}

// apply checks and then applies rules in one nft transaction.
func (a *Agent) apply(ctx context.Context, rules NodeRules) error {
	h := rules.Hash()
	if a.applied == h {
		return nil
	}
	script, err := Render(rules)
	if err != nil {
		return err
	}
	if _, err := a.NFT.Run(ctx, script, "--check", "-f", "-"); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	if _, err := a.NFT.Run(ctx, script, "-f", "-"); err != nil {
		a.applied = ""
		return err
	}
	a.applied = h
	return nil
}

func (a *Agent) status(state, message, seen string) NodeStatus {
	st := a.state
	s := NodeStatus{
		State: state, Seen: seen, Confirmed: st.Confirmed.Revision, RolledBack: st.RolledBack, RolledBackAt: st.RolledBackAt,
		Message: message, UpdatedAt: a.now().UTC().Truncate(time.Second), Version: a.Version,
	}
	if p := st.Pending; p != nil {
		dl := p.AppliedAt.Add(a.window()).UTC()
		s.Pending, s.Deadline = p.Revision, &dl
	}
	return s
}
