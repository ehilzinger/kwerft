// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package firewall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNFT stands in for nft on a host the installer prepared: it keeps the
// two managed chains' rules and lists them like `nft list table`.
type fakeNFT struct {
	prepared  bool
	chains    map[string][]string
	applied   []string // scripts applied with -f
	failCheck func(script string) bool
	failList  bool
}

func newFakeNFT() *fakeNFT {
	return &fakeNFT{prepared: true, chains: map[string][]string{ChainSSH: nil, ChainOpen: nil}}
}

func (f *fakeNFT) Run(_ context.Context, stdin string, args ...string) (string, error) {
	switch {
	case len(args) > 0 && args[0] == "list":
		if f.failList || !f.prepared {
			return "", errors.New("Error: No such file or directory")
		}
		var b strings.Builder
		b.WriteString("table inet kwerft {\n")
		for _, c := range []string{ChainSSH, ChainOpen} {
			b.WriteString("\tchain " + c + " {\n")
			for _, r := range f.chains[c] {
				b.WriteString("\t\t" + strings.Replace(r, "counter ", "counter packets 0 bytes 0 ", 1) + "\n")
			}
			b.WriteString("\t}\n\n")
		}
		b.WriteString("\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\tct state established,related accept\n" +
			"\t\ttcp dport 22 jump managed_ssh\n\t\ttcp dport { 22, 80, 443 } accept\n\t\tjump managed_open\n\t}\n}\n")
		return b.String(), nil
	case len(args) > 0 && args[0] == "--check":
		if !f.prepared {
			return "", errors.New("Error: No such file or directory; did you mean table 'kwerft' in family inet?")
		}
		if f.failCheck != nil && f.failCheck(stdin) {
			return "", errors.New("Error: syntax error")
		}
		return "", nil
	case len(args) > 0 && args[0] == "-f":
		next := map[string][]string{ChainSSH: f.chains[ChainSSH], ChainOpen: f.chains[ChainOpen]}
		for _, line := range strings.Split(strings.TrimSpace(stdin), "\n") {
			if c, ok := strings.CutPrefix(line, "flush chain inet kwerft "); ok {
				next[c] = nil
				continue
			}
			rest, ok := strings.CutPrefix(line, "add rule inet kwerft ")
			if !ok {
				return "", errors.New("unexpected line " + line)
			}
			c, rule, _ := strings.Cut(rest, " ")
			next[c] = append(next[c], rule)
		}
		f.chains = next
		f.applied = append(f.applied, stdin)
		return "", nil
	}
	return "", errors.New("unexpected nft call")
}

// holds reports whether the kernel holds rules.
func (f *fakeNFT) holds(rules NodeRules) bool {
	listing, _ := f.Run(context.Background(), "", "list")
	return ParseHost(listing).Holds(rules.Hash())
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newAgent(t *testing.T, nft *fakeNFT, c *clock, dir string) *Agent {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	return &Agent{Node: "n1", NFT: nft, Dir: dir, Now: c.Now}
}

func desired(rev, confirmed string, rules NodeRules) *Desired {
	return &Desired{Revision: rev, Confirmed: confirmed, Nodes: map[string]NodeRules{"n1": rules}}
}

var (
	narrow   = NodeRules{SSHSources: []string{"203.0.113.0/24"}}
	withPort = NodeRules{Open: []Open{{Rule: "web", Protocol: "tcp", Port: 8080}}}
)

func TestAgentBaselineOnANewNode(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	st := a.Step(t.Context(), desired("r0", "", NodeRules{}))
	if st.State != StateInSync || st.Confirmed != "r0" || st.Seen != "r0" {
		t.Fatalf("status: %+v", st)
	}
	if !nft.holds(NodeRules{}) {
		t.Fatal("the empty rules (with markers) were not applied")
	}
	n := len(nft.applied)
	a.Step(t.Context(), desired("r0", "", NodeRules{}))
	if len(nft.applied) != n {
		t.Fatal("re-applied unchanged rules")
	}
}

func TestAgentConfirmedChange(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	a.Step(t.Context(), desired("r0", "r0", NodeRules{}))

	st := a.Step(t.Context(), desired("r1", "r0", narrow))
	if st.State != StatePending || st.Pending != "r1" || st.Deadline == nil || !st.Deadline.Equal(c.t.Add(ConfirmWindow)) {
		t.Fatalf("narrowing SSH must wait for confirmation: %+v", st)
	}
	if !nft.holds(narrow) {
		t.Fatal("the pending rules are not applied")
	}
	c.Advance(30 * time.Second)
	if st := a.Step(t.Context(), desired("r1", "r0", narrow)); st.State != StatePending {
		t.Fatalf("still pending: %+v", st)
	}
	st = a.Step(t.Context(), desired("r1", "r1", narrow))
	if st.State != StateInSync || st.Confirmed != "r1" || st.Pending != "" {
		t.Fatalf("confirmed: %+v", st)
	}
	c.Advance(time.Hour)
	if st := a.Step(t.Context(), nil); st.State != StateInSync || !nft.holds(narrow) {
		t.Fatalf("a confirmed change stays: %+v", st)
	}
}

func TestAgentRollsBackWithoutConfirmation(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	a.Step(t.Context(), desired("r0", "r0", withPort))
	a.Step(t.Context(), desired("r1", "r0", narrow)) // narrows SSH and closes 8080
	c.Advance(ConfirmWindow - time.Second)
	a.Step(t.Context(), nil)
	if !nft.holds(narrow) {
		t.Fatal("rolled back early")
	}
	// The console is gone: no desired state at all, only time passes.
	c.Advance(time.Second)
	st := a.Step(t.Context(), nil)
	if !nft.holds(withPort) {
		t.Fatal("not rolled back to the confirmed rules")
	}
	if st.RolledBack != "r1" || st.RolledBackAt == nil || st.Pending != "" {
		t.Fatalf("status: %+v", st)
	}
	// The same revision is not tried again …
	st = a.Step(t.Context(), desired("r1", "r0", narrow))
	if st.State != StateRolledBack || !nft.holds(withPort) {
		t.Fatalf("re-applied a rolled back revision: %+v", st)
	}
	// … not even when a late confirmation arrives.
	st = a.Step(t.Context(), desired("r1", "r1", narrow))
	if st.State != StateRolledBack || !nft.holds(withPort) {
		t.Fatalf("a late confirmation applied rolled back rules: %+v", st)
	}
	// "Apply again" makes a new revision of the same rules.
	st = a.Step(t.Context(), desired("r2", "r0", narrow))
	if st.State != StatePending || !nft.holds(narrow) {
		t.Fatalf("apply again: %+v", st)
	}
}

func TestAgentDiscardRestoresAtOnce(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	a.Step(t.Context(), desired("r0", "r0", NodeRules{}))
	a.Step(t.Context(), desired("r1", "r0", narrow))
	// "Roll back now" restores the rules: the revision is the confirmed one again.
	st := a.Step(t.Context(), desired("r0", "r0", NodeRules{}))
	if st.State != StateInSync || st.Pending != "" || !nft.holds(NodeRules{}) {
		t.Fatalf("discard: %+v", st)
	}
}

func TestAgentAppliesWideningAtOnce(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	a.Step(t.Context(), desired("r0", "r0", narrow))
	st := a.Step(t.Context(), desired("r1", "r0", NodeRules{SSHSources: narrow.SSHSources, Open: withPort.Open}))
	if st.State != StateInSync || st.Confirmed != "r1" {
		t.Fatalf("opening a port takes nothing away: %+v", st)
	}
	st = a.Step(t.Context(), desired("r2", "r0", withPort))
	if st.State != StateInSync || !nft.holds(withPort) {
		t.Fatalf("lifting the SSH narrowing takes nothing away: %+v", st)
	}
}

func TestAgentRollsBackAfterARestart(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	dir := t.TempDir()
	a := newAgent(t, nft, c, dir)
	a.Step(t.Context(), desired("r0", "r0", NodeRules{}))
	a.Step(t.Context(), desired("r1", "r0", narrow))

	// The agent restarts after the window ran out, with the API unreachable.
	c.Advance(2 * ConfirmWindow)
	b := newAgent(t, nft, c, dir)
	st := b.Step(t.Context(), nil)
	if st.RolledBack != "r1" || !nft.holds(NodeRules{}) {
		t.Fatalf("restart: %+v", st)
	}
}

func TestAgentConfirmedElsewhereAppliesDirectly(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	// A node joining a cluster whose narrowing was confirmed long ago.
	st := a.Step(t.Context(), desired("r5", "r5", narrow))
	if st.State != StateInSync || !nft.holds(narrow) {
		t.Fatalf("new node: %+v", st)
	}
}

func TestAgentReappliesAfterTheChainsWereEmptied(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	a.Step(t.Context(), desired("r0", "r0", withPort))
	nft.chains = map[string][]string{ChainSSH: nil, ChainOpen: nil} // installer re-run
	a.Step(t.Context(), nil)
	if !nft.holds(withPort) {
		t.Fatal("not re-applied")
	}
}

func TestAgentLeavesAnUnpreparedHostAlone(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	nft.prepared = false
	a := newAgent(t, nft, c, "")
	st := a.Step(t.Context(), desired("r1", "", withPort))
	if st.State != StateUnprepared || len(nft.applied) != 0 || !strings.Contains(st.Message, "Re-run the installer") {
		t.Fatalf("status: %+v", st)
	}
}

func TestAgentPaused(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	dir := t.TempDir()
	a := newAgent(t, nft, c, dir)
	a.Step(t.Context(), desired("r0", "r0", narrow))
	if err := os.WriteFile(filepath.Join(dir, PausedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := a.Step(t.Context(), desired("r0", "r0", narrow))
	if st.State != StatePaused || !nft.holds(NodeRules{}) {
		t.Fatalf("paused: %+v", st)
	}
	// --reset-firewall deleted the table: nothing to clear, still paused.
	nft.prepared = false
	if st := a.Step(t.Context(), nil); st.State != StatePaused {
		t.Fatalf("paused without a table: %+v", st)
	}
	nft.prepared = true
	if err := os.Remove(filepath.Join(dir, PausedFile)); err != nil {
		t.Fatal(err)
	}
	if st := a.Step(t.Context(), nil); st.State != StateInSync || !nft.holds(narrow) {
		t.Fatalf("resumed: %+v", st)
	}
}

func TestAgentKeepsTheConfirmedRulesWhenNftRefusesAChange(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	a.Step(t.Context(), desired("r0", "r0", withPort))
	nft.failCheck = func(s string) bool { return strings.Contains(s, "203.0.113.0/24") }
	st := a.Step(t.Context(), desired("r1", "r0", narrow))
	if st.State != StateError || !strings.Contains(st.Message, "refused") || !nft.holds(withPort) {
		t.Fatalf("status: %+v", st)
	}
	if st := a.Step(t.Context(), desired("r1", "r0", narrow)); st.State != StateError || st.Pending != "" {
		t.Fatalf("retried a refused change: %+v", st)
	}
}

func TestAgentIgnoresOtherNodes(t *testing.T) {
	nft, c := newFakeNFT(), &clock{t: time.Unix(1e9, 0)}
	a := newAgent(t, nft, c, "")
	a.Step(t.Context(), desired("r0", "r0", NodeRules{}))
	st := a.Step(t.Context(), &Desired{Revision: "r1", Nodes: map[string]NodeRules{"n2": narrow}})
	if st.State != StateInSync || st.Pending != "" || !nft.holds(NodeRules{}) {
		t.Fatalf("status: %+v", st)
	}
}
