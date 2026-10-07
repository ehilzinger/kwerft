// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	ordered := []string{"0.1.0-rc.1", "0.1.0-rc.2", "0.1.0-rc.10", "0.1.0", "v0.2.0-alpha", "0.2.0-alpha.1", "0.2.0-beta", "0.2.0", "0.10.0", "1.0.0"}
	for i := range ordered {
		for j := range ordered {
			got := compareVersions(ordered[i], ordered[j])
			want := sign(i - j)
			if got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	for _, bad := range []string{"", "latest", "1.2", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3+build", "1.2.3-a..b"} {
		if validVersion(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPreviousAndLatestStable(t *testing.T) {
	tags := "v0.4.0\nv0.5.0-rc.1\nv0.3.0\nv0.1.0-rc.2\nv0.2.0\nnot-a-tag\nv0.5.0\nv0.6.0-rc.1\n"
	for target, want := range map[string]string{
		"0.5.0": "0.4.0", "v0.5.0": "0.4.0", "0.5.0-rc.1": "0.4.0", "0.6.0-rc.1": "0.5.0", "0.4.0": "0.3.0", "0.1.0": "",
	} {
		got, err := previousStable(strings.NewReader(tags), target)
		if err != nil || got != want {
			t.Errorf("previous(%s) = %q, %v; want %q", target, got, err, want)
		}
	}
	if got, _ := latestStable(strings.NewReader(tags)); got != "0.5.0" {
		t.Errorf("latest = %q", got)
	}
	var out bytes.Buffer
	if err := cmdVersions("previous", []string{"-version", "v0.5.0"}, strings.NewReader(tags), &out); err != nil || out.String() != "0.4.0\n" {
		t.Errorf("previous command: %q %v", out.String(), err)
	}
	out.Reset()
	if err := cmdVersions("previous", []string{"-version", "0.1.0"}, strings.NewReader(tags), &out); err != nil || out.String() != "" {
		t.Errorf("no previous release should print nothing: %q %v", out.String(), err)
	}
	// A console upgrade starts from the release just before, candidates too.
	for target, want := range map[string]string{
		"0.6.0-rc.2": "0.6.0-rc.1", "0.6.0-rc.1": "0.5.0", "0.6.0": "0.6.0-rc.1", "0.5.0": "0.5.0-rc.1", "0.5.0-rc.1": "0.4.0",
	} {
		out.Reset()
		if err := cmdVersions("previous", []string{"-prereleases", "-version", target}, strings.NewReader(tags), &out); err != nil || out.String() != want+"\n" {
			t.Errorf("previous -prereleases %s = %q, %v; want %s", target, out.String(), err, want)
		}
	}
}

func TestSweep(t *testing.T) {
	f := newFakeCloud(t)
	c := f.client()
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-4 * time.Hour)
	f.mu.Lock()
	add := func(id int64, name string, created time.Time, labels map[string]string) {
		s := &hServer{ID: id, Name: name, Created: created, Labels: labels}
		f.servers[id] = s
		f.keys[id+1000] = &hSSHKey{ID: id + 1000, Name: name, Created: created, Labels: labels}
	}
	add(1, "kwerft-e2e-old", old, map[string]string{labelE2E: "true", labelRun: "1-1-fresh"})
	add(2, "kwerft-e2e-running", now.Add(-10*time.Minute), map[string]string{labelE2E: "true", labelRun: "2-1-fresh"})
	add(3, "someones-server", old, map[string]string{"team": "ops"})
	add(4, "kwerft-e2e-false", old, map[string]string{labelE2E: "false"})
	f.mu.Unlock()

	res, err := sweep(ctx, c, now, 3*time.Hour, "", true)
	if err != nil || len(res.Deleted) != 2 || !strings.HasPrefix(res.Deleted[0], "would delete server kwerft-e2e-old") {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if s, k := f.counts(); s != 4 || k != 4 {
		t.Fatalf("a dry run deleted something: %d %d", s, k)
	}

	res, err = sweep(ctx, c, now, 3*time.Hour, "", false)
	if err != nil || len(res.Deleted) != 2 || res.Kept != 2 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	f.mu.Lock()
	_, oldLeft := f.servers[1]
	_, running := f.servers[2]
	_, foreign := f.servers[3]
	_, falseLabel := f.servers[4]
	f.mu.Unlock()
	if oldLeft || !running || !foreign || !falseLabel {
		t.Errorf("old gone %v, running kept %v, foreign kept %v, kwerft-e2e=false kept %v", !oldLeft, running, foreign, falseLabel)
	}

	// By run: everything of that run, whatever its age.
	res, err = sweep(ctx, c, now, 3*time.Hour, "2-1-fresh", false)
	if err != nil || len(res.Deleted) != 2 {
		t.Fatalf("sweep by run: %+v %v", res, err)
	}
	if s, k := f.counts(); s != 2 || k != 2 {
		t.Errorf("left %d servers, %d keys; want only the two foreign ones", s, k)
	}
}

func TestCreatedAtFallsBackToTheLabel(t *testing.T) {
	if got := createdAt(time.Time{}, map[string]string{labelCreated: "1700000000"}); !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("got %v", got)
	}
	if !createdAt(time.Time{}, nil).IsZero() {
		t.Error("no timestamp should be zero")
	}
}

func TestHcloudRetriesReadsNotCreates(t *testing.T) {
	var gets, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		} else if gets.Add(1) < 3 {
			apiErr(w, 503, "unavailable", "try later")
			return
		}
		if r.Method == http.MethodPost {
			apiErr(w, 503, "unavailable", "try later")
			return
		}
		writeJSONTest(w, 200, map[string]any{"server": map[string]any{"id": 5, "status": "running"}})
	}))
	defer srv.Close()
	c := newHcloud("t", srv.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	s, err := c.server(context.Background(), 5)
	if err != nil || s.ID != 5 || gets.Load() != 3 {
		t.Errorf("GET: %+v %v after %d tries", s, err, gets.Load())
	}
	_, err = c.createServer(context.Background(), createServerRequest{Name: "x"})
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 503 || posts.Load() != 1 {
		t.Errorf("POST: %v after %d tries (creates are not retried)", err, posts.Load())
	}
}

func TestHcloudNotFoundAndAuth(t *testing.T) {
	f := newFakeCloud(t)
	c := f.client()
	if _, err := c.server(context.Background(), 999); !errors.Is(err, errNotFound) {
		t.Errorf("missing server: %v", err)
	}
	if err := c.deleteServer(context.Background(), 999); err != nil {
		t.Errorf("deleting a missing server is not an error: %v", err)
	}
	c.token = "wrong"
	_, err := c.servers(context.Background(), "kwerft-e2e=true")
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("bad token: %v", err)
	}
}

func TestCertChecker(t *testing.T) {
	staging := newTestCA(t, "(STAGING) Let's Encrypt")
	public := newTestCA(t, "Let's Encrypt")
	selfSigned := newTestCA(t, "TRAEFIK DEFAULT CERT")
	pool := x509.NewCertPool()
	pool.AddCert(public.cert)

	state := func(ca *testCA, host string, names ...string) tls.ConnectionState {
		c := ca.issue(t, names...)
		leaf, _ := x509.ParseCertificate(c.Certificate[0])
		return tls.ConnectionState{ServerName: host, PeerCertificates: []*x509.Certificate{leaf}}
	}
	cc := &certChecker{roots: pool}
	if err := cc.verify(state(public, "a.example.com", "a.example.com")); err != nil {
		t.Errorf("trusted certificate refused: %v", err)
	}
	if err := cc.verify(state(staging, "b.example.com", "*.example.com")); err != nil {
		t.Errorf("staging certificate refused: %v", err)
	}
	if got := cc.issuerOf("b.example.com"); !strings.Contains(got, "(STAGING)") {
		t.Errorf("issuer %q", got)
	}
	if err := cc.verify(state(staging, "c.other.com", "b.example.com")); err == nil {
		t.Error("staging certificate for another name accepted")
	}
	if err := cc.verify(state(selfSigned, "a.example.com", "a.example.com")); err == nil {
		t.Error("untrusted, non-staging certificate accepted")
	}
	expired := &certChecker{roots: pool, now: func() time.Time { return time.Now().Add(48 * time.Hour) }}
	if err := expired.verify(state(staging, "b.example.com", "b.example.com")); err == nil {
		t.Error("expired staging certificate accepted")
	}
}

func TestReport(t *testing.T) {
	start := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	r := &report{Title: "Kwerft e2e", Scenario: "Fresh install of v0.5.0", Started: start, Finished: start.Add(70 * time.Minute),
		PriceHourly: "0.0136", Server: "cx33", CleanupOK: true, Cleanup: []string{"server 1 deleted"}}
	r.add(result{Name: "Install v0.5.0", Status: pass, Duration: 8*time.Minute + 3*time.Second, Detail: "exit 0 | fine"})
	r.add(result{Name: "Crash loop alert", Status: warn, Duration: 3 * time.Minute})
	var b bytes.Buffer
	r.markdown(&b)
	md := b.String()
	for _, want := range []string{"## Kwerft e2e: passed", "Fresh install of v0.5.0 · 70m 00s in total", "| Install v0.5.0 | pass | 8m 03s | exit 0 \\| fine |", "| Crash loop alert | slow |", "€0.0272 (2 started hour(s)"} {
		if !strings.Contains(md, want) {
			t.Errorf("summary lacks %q:\n%s", want, md)
		}
	}
	r.CleanupOK = false
	if r.passed() {
		t.Error("a run with leftovers passed")
	}
	r.CleanupOK = true
	r.add(result{Name: "Metrics", Status: fail})
	if r.passed() {
		t.Error("a run with a failed check passed")
	}
}

func TestTailAndPrefix(t *testing.T) {
	tl := newTail(2)
	_, _ = tl.Write([]byte("one\ntwo\nthr"))
	_, _ = tl.Write([]byte("ee\nfour"))
	_, _ = tl.Write([]byte("\nfive\n"))
	if got := tl.String(); got != "four\nfive" {
		t.Errorf("tail %q", got)
	}
	var b bytes.Buffer
	w := prefixWriter(&b, "> ")
	_, _ = w.Write([]byte("a\nb"))
	_, _ = w.Write([]byte("c\n"))
	if b.String() != "> a\n> bc\n" {
		t.Errorf("prefixed %q", b.String())
	}
}

func TestValidLabelValue(t *testing.T) {
	for v, want := range map[string]bool{"123-1-fresh": true, "a": true, "-a": false, "a-": false, "a b": false, "": false, "a.b_c": true} {
		if validLabelValue(v) != want {
			t.Errorf("%q: %v", v, !want)
		}
	}
}
