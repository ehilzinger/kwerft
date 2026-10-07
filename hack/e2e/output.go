// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// tail keeps the last n lines written to it.
type tail struct {
	mu    sync.Mutex
	n     int
	lines []string
	part  []byte
}

func newTail(n int) *tail { return &tail{n: n} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.part = append(t.part, p...)
	for {
		i := bytes.IndexByte(t.part, '\n')
		if i < 0 {
			break
		}
		t.lines = append(t.lines, string(t.part[:i]))
		t.part = t.part[i+1:]
		if len(t.lines) > t.n {
			t.lines = t.lines[len(t.lines)-t.n:]
		}
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := strings.Join(t.lines, "\n")
	if len(t.part) > 0 {
		out += "\n" + string(t.part)
	}
	return out
}

// prefixWriter starts every line written to w with prefix.
type prefixed struct {
	w      io.Writer
	prefix string
	mid    bool // inside a line
}

func prefixWriter(w io.Writer, prefix string) io.Writer { return &prefixed{w: w, prefix: prefix} }

func (p *prefixed) Write(b []byte) (int, error) {
	var out bytes.Buffer
	for _, c := range b {
		if !p.mid {
			out.WriteString(p.prefix)
			p.mid = true
		}
		out.WriteByte(c)
		if c == '\n' {
			p.mid = false
		}
	}
	if _, err := p.w.Write(out.Bytes()); err != nil {
		return 0, err
	}
	return len(b), nil
}
