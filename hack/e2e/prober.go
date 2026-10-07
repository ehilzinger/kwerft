// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// prober requests an App's URL over and over while an upgrade runs and
// measures the longest time it did not answer: from the last good answer
// to the next one (or to the end, if it never came back).
type prober struct {
	url, want string
	every     time.Duration
	get       func(ctx context.Context, url string) (int, string, error)
	now       func() time.Time

	mu      sync.Mutex
	started time.Time
	lastOK  time.Time
	down    bool // the last probe failed
	longest time.Duration
	total   int
	failed  int
	lastErr string

	cancel context.CancelFunc
	done   chan struct{}
}

// probeResult is what a prober saw.
type probeResult struct {
	URL       string
	Longest   time.Duration
	Total     int
	Failed    int
	LastError string
	Duration  time.Duration
}

func (p probeResult) String() string {
	s := fmt.Sprintf("%s: longest gap %s, %d of %d request(s) failed over %s", p.URL, fmtGap(p.Longest), p.Failed, p.Total, fmtDuration(p.Duration))
	if p.LastError != "" {
		s += " (last: " + p.LastError + ")"
	}
	return s
}

func fmtGap(d time.Duration) string {
	if d <= 0 {
		return "none"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return fmtDuration(d)
}

func (r *runner) startProbe(ctx context.Context, url, want string) *prober {
	ctx, cancel := context.WithCancel(ctx)
	p := &prober{url: url, want: want, every: r.cfg.ProbeEvery, get: r.console.get, now: r.now, cancel: cancel, done: make(chan struct{})}
	if p.every <= 0 {
		p.every = r.cfg.Poll
	}
	p.started, p.lastOK = r.now(), r.now()
	go p.loop(ctx)
	return p
}

func (p *prober) loop(ctx context.Context) {
	defer close(p.done)
	for {
		p.probe(ctx)
		if sleepCtx(ctx, p.every) != nil {
			return
		}
	}
}

func (p *prober) probe(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	code, body, err := p.get(rctx, p.url)
	cancel()
	if ctx.Err() != nil {
		return // stopped mid-request: not the App's fault
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total++
	now := p.now()
	if err == nil && code == http.StatusOK && strings.Contains(body, p.want) {
		if p.down {
			p.longest = max(p.longest, now.Sub(p.lastOK))
		}
		p.lastOK, p.down = now, false
		return
	}
	p.failed++
	p.down = true
	if err != nil {
		p.lastErr = err.Error()
	} else {
		p.lastErr = fmt.Sprintf("HTTP %d: %s", code, truncate(strings.TrimSpace(body), 80))
	}
}

// finish stops probing and returns the result.
func (p *prober) finish() probeResult {
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	longest := p.longest
	if p.down {
		longest = max(longest, now.Sub(p.lastOK))
	}
	res := probeResult{URL: p.url, Longest: longest, Total: p.total, Failed: p.failed, Duration: now.Sub(p.started)}
	if p.failed > 0 {
		res.LastError = p.lastErr
	}
	return res
}
