// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

type status string

const (
	pass status = "pass"
	fail status = "FAIL"
	warn status = "slow" // passed, but over its time budget
	skip status = "skipped"
)

// result is one check or step of a run.
type result struct {
	Name     string
	Status   status
	Duration time.Duration
	Detail   string
}

// report collects what a run did, for the job summary.
type report struct {
	mu sync.Mutex

	Title    string
	Scenario string // "fresh install of 0.5.0" / "upgrade 0.4.0 → 0.5.0"
	Started  time.Time
	Finished time.Time
	Results  []result

	Server      string // "cx33 in nbg1, ubuntu-26.04 (id 123, 203.0.113.10)"
	PriceHourly string // gross EUR per hour, from the API
	// Billing: every server of the run; when set, the cost is their sum.
	Billing   []billed
	Cleanup   []string
	CleanupOK bool
	Notes     []string
	// Log is the tail of the installer output or diagnostics of a failure.
	Log string
}

func (r *report) add(res result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Results = append(r.Results, res)
}

func (r *report) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// passed: every check passed (slow ones count as passed) and the server is gone.
func (r *report) passed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.CleanupOK {
		return false
	}
	for _, res := range r.Results {
		if res.Status == fail {
			return false
		}
	}
	return len(r.Results) > 0
}

// billed is one server's lifetime and price (gross EUR per hour).
type billed struct {
	Price      string
	Start, End time.Time
}

// cost estimates the bill: Hetzner charges each started hour, per server.
func (r *report) cost() string {
	if len(r.Billing) == 0 {
		p, err := strconv.ParseFloat(r.PriceHourly, 64)
		if err != nil || r.Finished.IsZero() {
			return ""
		}
		hours := math.Max(1, math.Ceil(r.Finished.Sub(r.Started).Hours()))
		return fmt.Sprintf("€%.4f (%.0f started hour(s) at €%.4f/h incl. VAT, plus the IPv4 address)", p*hours, hours, p)
	}
	total, hours := 0.0, 0.0
	for _, b := range r.Billing {
		p, err := strconv.ParseFloat(b.Price, 64)
		if err != nil {
			return ""
		}
		h := math.Max(1, math.Ceil(b.End.Sub(b.Start).Hours()))
		total += p * h
		hours += h
	}
	if len(r.Billing) == 1 {
		return fmt.Sprintf("€%.4f (%.0f started hour(s) at €%.4f/h incl. VAT, plus the IPv4 address)", total, hours, total/hours)
	}
	return fmt.Sprintf("€%.4f (%d servers, %.0f started server-hour(s) incl. VAT, plus their IPv4 addresses)", total, len(r.Billing), hours)
}

func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

// markdown renders the report for $GITHUB_STEP_SUMMARY.
func (r *report) markdown(w io.Writer) {
	verdict := "passed"
	if !r.passed() {
		verdict = "FAILED"
	}
	fmt.Fprintf(w, "## %s: %s\n\n", r.Title, verdict)
	if r.Scenario != "" {
		fmt.Fprintf(w, "%s", r.Scenario)
		if !r.Finished.IsZero() {
			fmt.Fprintf(w, " · %s in total", fmtDuration(r.Finished.Sub(r.Started)))
		}
		fmt.Fprint(w, "\n\n")
	}
	if len(r.Results) > 0 {
		fmt.Fprintln(w, "| Check | Result | Time | Details |")
		fmt.Fprintln(w, "|---|---|---|---|")
		for _, res := range r.Results {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", mdCell(res.Name), res.Status, fmtDuration(res.Duration), mdCell(res.Detail))
		}
		fmt.Fprintln(w)
	}
	if r.Server != "" {
		fmt.Fprintf(w, "- Server: %s\n", r.Server)
	}
	if c := r.cost(); c != "" {
		fmt.Fprintf(w, "- Cost: %s\n", c)
	}
	if len(r.Cleanup) > 0 {
		state := "done"
		if !r.CleanupOK {
			state = "**incomplete — the sweeper removes leftovers older than 3 h; check the Hetzner test project**"
		}
		fmt.Fprintf(w, "- Cleanup %s: %s\n", state, strings.Join(r.Cleanup, "; "))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "- %s\n", n)
	}
	if r.Log != "" {
		fmt.Fprintf(w, "\n<details><summary>Last lines of the log</summary>\n\n```\n%s\n```\n\n</details>\n", strings.TrimRight(r.Log, "\n"))
	}
}
