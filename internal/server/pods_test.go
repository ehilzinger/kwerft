// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilexec "k8s.io/client-go/util/exec"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Unit tests for replicas, logs, shells and recordings against a fake
// Kubernetes: envtest has no kubelet, so it cannot serve logs or exec. The
// RBAC side is tested against a real API server in pods_rbac_test.go.

// ---- fake Kubernetes ------------------------------------------------------------

type fakePods struct {
	mu      sync.Mutex
	pods    []corev1.Pod
	metrics map[string]podUsage
	denied  map[string]bool // "get/log", "create/exec"
	// backlog lines per pod (non-follow requests), already with timestamps.
	backlog map[string][]string
	// follow streams: a pipe per follow request, by pod.
	follows map[string][]*io.PipeWriter
	opened  atomic.Int32
	closed  atomic.Int32
	calls   []corev1.PodLogOptions
	exec    func(ctx context.Context, opts *corev1.PodExecOptions, s execStreams) error
	// debugState is the status a debug toolbox gets; nil means running.
	debugState *corev1.ContainerState
	// taskPhases sets a Task's phase (default Running); "missing" is not found.
	taskPhases map[string]kwerftv1.TaskPhase
}

func newFakePods(pods ...corev1.Pod) *fakePods {
	return &fakePods{pods: pods, denied: map[string]bool{}, backlog: map[string][]string{}, follows: map[string][]*io.PipeWriter{}}
}

func (f *fakePods) app(_ context.Context, project, name string) error {
	if name == "missing" {
		return apierrors.NewNotFound(schema.GroupResource{Group: "kwerft.dev", Resource: "apps"}, name)
	}
	return nil
}

func (f *fakePods) task(_ context.Context, project, name string) (*kwerftv1.Task, error) {
	if name == "missing" {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "kwerft.dev", Resource: "tasks"}, name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	phase, ok := f.taskPhases[name]
	if !ok {
		phase = kwerftv1.TaskRunning
	}
	return &kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}, Status: kwerftv1.TaskStatus{Phase: phase}}, nil
}

func (f *fakePods) listPods(_ context.Context, ns string, sel labels.Selector) ([]corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []corev1.Pod
	for _, p := range f.pods {
		if p.Namespace == ns && sel.Matches(labels.Set(p.Labels)) {
			out = append(out, *p.DeepCopy())
		}
	}
	return out, nil
}

func (f *fakePods) getPod(_ context.Context, ns, name string) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pods {
		if p.Namespace == ns && p.Name == name {
			return p.DeepCopy(), nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

func (f *fakePods) podMetrics(context.Context, string, labels.Selector) (map[string]podUsage, error) {
	if f.metrics == nil {
		return nil, apierrors.NewServiceUnavailable("metrics.k8s.io is not available")
	}
	return f.metrics, nil
}

func (f *fakePods) allowed(_ context.Context, _, verb, sub, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.denied[verb+"/"+sub], nil
}

type trackedReader struct {
	io.ReadCloser
	f    *fakePods
	once sync.Once
}

func (r *trackedReader) Close() error {
	r.once.Do(func() { r.f.closed.Add(1) })
	return r.ReadCloser.Close()
}

func (f *fakePods) logs(_ context.Context, _, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, *opts)
	f.opened.Add(1)
	if !opts.Follow {
		body := strings.Join(f.backlog[pod], "\n")
		if body != "" {
			body += "\n"
		}
		return &trackedReader{ReadCloser: io.NopCloser(strings.NewReader(body)), f: f}, nil
	}
	pr, pw := io.Pipe()
	f.follows[pod] = append(f.follows[pod], pw)
	return &trackedReader{ReadCloser: pr, f: f}, nil
}

// write sends a line to the latest follow stream of pod, waiting for it.
func (f *fakePods) write(t *testing.T, pod, line string) {
	t.Helper()
	var pw *io.PipeWriter
	eventually(t, func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if ws := f.follows[pod]; len(ws) > 0 {
			pw = ws[len(ws)-1]
			return nil
		}
		return fmt.Errorf("no follow stream for %s", pod)
	})
	if _, err := io.WriteString(pw, line+"\n"); err != nil {
		t.Fatalf("write to %s: %v", pod, err)
	}
}

func (f *fakePods) followCalls() []corev1.PodLogOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []corev1.PodLogOptions
	for _, c := range f.calls {
		if c.Follow {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakePods) execIn(ctx context.Context, _, _ string, opts *corev1.PodExecOptions, s execStreams) error {
	return f.exec(ctx, opts, s)
}

// fakeBackend adapts fakePods (exec has a different name to keep the
// fake's exec hook a plain field).
type fakeBackend struct{ *fakePods }

func (b fakeBackend) exec(ctx context.Context, ns, pod string, opts *corev1.PodExecOptions, s execStreams) error {
	return b.execIn(ctx, ns, pod, opts, s)
}

func appPod(name, app string, created time.Time, running bool) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{controllers.LabelApp: app, controllers.LabelProject: "shop"}},
		Spec:   corev1.PodSpec{NodeName: "fsn1-node-1", Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if running {
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(created)}}}}
	}
	return p
}

// ---- a console wired to the fake ----------------------------------------------

type podEnv struct {
	*env
	fake *fakePods
	pods *podsAPI
	dir  string
}

func fastLogLimits() logLimits {
	l := defaultLogLimits
	l.poll, l.heartbeat, l.sessionCheck = 50*time.Millisecond, time.Second, time.Second
	return l
}

func newPodEnv(t *testing.T, fake *fakePods, tune ...func(*podsAPI)) *podEnv {
	t.Helper()
	pe := &podEnv{fake: fake, dir: filepath.Join(t.TempDir(), "recordings")}
	pe.env = newEnv(t, func(c *Config) {
		c.RecordingsDir = pe.dir
		c.podsHook = func(p *podsAPI) {
			p.backend = func(context.Context, *principal) (podBackend, error) { return fakeBackend{fake}, nil }
			p.logs = fastLogLimits()
			for _, f := range tune {
				f(p)
			}
			pe.pods = p
		}
	})
	pe.completeSetup(t)
	t.Cleanup(func() { pe.waitIdle(t) })
	return pe
}

// waitIdle waits until every stream and session has cleaned up.
func (pe *podEnv) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pe.pods.running.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d streams or sessions still running 5s after the client left", pe.pods.running.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// asViewer adds a viewer and returns a client signed in as them.
func (pe *podEnv) asRole(t *testing.T, role string) *env {
	t.Helper()
	email := role + "@example.com"
	if err := pe.store.CreateUser(context.Background(), &store.User{Email: email, Name: role, PasswordHash: mustHash(t, "a long test password"), Role: role}); err != nil {
		t.Fatal(err)
	}
	c := pe.newClient()
	if code, _ := c.call(t, "POST", "/api/v1/session", map[string]string{"email": email, "password": "a long test password"}, nil); code != http.StatusOK {
		t.Fatalf("sign in %s: %d", role, code)
	}
	return c
}

type sse struct {
	name string
	data map[string]any
}

// stream opens a log stream and returns its events and a cancel func.
func (pe *podEnv) stream(t *testing.T, path string) (<-chan sse, context.CancelFunc, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", pe.srv.URL+path, nil)
	res, err := pe.client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, cancel, res
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
	events := make(chan sse, 1000)
	go func() {
		defer close(events)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var ev sse
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data)
			case line == "" && ev.name != "":
				events <- ev
				ev = sse{}
			}
		}
	}()
	return events, cancel, res
}

// next returns the next event that is not a status update.
func next(t *testing.T, events <-chan sse, names ...string) sse {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("stream closed while waiting for %v", names)
			}
			if slices.Contains(names, ev.name) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %v event within 5s", names)
		}
	}
}

func ts(base time.Time, ms int) string {
	return base.Add(time.Duration(ms) * time.Millisecond).UTC().Format(time.RFC3339Nano)
}

// ---- line handling -------------------------------------------------------------

func TestReadLinesCutsLongLinesAndKeepsGoing(t *testing.T) {
	huge := strings.Repeat("x", 100<<10) // longer than the reader's buffer
	in := "short\r\n" + strings.Repeat("y", 40) + "\n" + huge + "\nafter\nno newline at the end"
	type got struct {
		text string
		cut  bool
	}
	var lines []got
	if err := readLines(strings.NewReader(in), 10, func(l []byte, cut bool) bool {
		lines = append(lines, got{string(l), cut})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	want := []got{{"short", false}, {"yyyyyyyyyy", true}, {"xxxxxxxxxx", true}, {"after", false}, {"no newline", true}}
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("lines = %v\nwant    %v", lines, want)
	}
	// fn can stop early.
	n := 0
	_ = readLines(strings.NewReader("a\nb\nc\n"), 10, func([]byte, bool) bool { n++; return n < 2 })
	if n != 2 {
		t.Errorf("read %d lines after stop, want 2", n)
	}
}

func TestParseLogLineSplitsTheTimestamp(t *testing.T) {
	ln := parseLogLine("web-1", "app", []byte("2026-10-04T09:14:02.381234567Z GET /healthz 200"), false)
	if ln.TS != "2026-10-04T09:14:02.381234567Z" || ln.Text != "GET /healthz 200" || ln.Pod != "web-1" || ln.Container != "app" {
		t.Errorf("parsed %+v", ln)
	}
	if ln := parseLogLine("p", "c", []byte("no timestamp here"), false); ln.TS != "" || ln.Text != "no timestamp here" {
		t.Errorf("without a timestamp: %+v", ln)
	}
	if ln := parseLogLine("p", "c", []byte("2026-10-04T09:14:02Z"), false); ln.TS == "" || ln.Text != "" {
		t.Errorf("empty line: %+v", ln)
	}
}

// ---- replicas ------------------------------------------------------------------

func TestReplicasEndpointShape(t *testing.T) {
	now := time.Now().Add(-time.Hour)
	crashing := appPod("web-b", "web", now.Add(time.Minute), false)
	crashing.Status.Phase = corev1.PodRunning
	crashing.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app", RestartCount: 4,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 2m40s"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(now.Add(30 * time.Minute))}},
	}}
	pending := appPod("web-c", "web", now.Add(2*time.Minute), false)
	other := appPod("api-a", "api", now, true)
	fake := newFakePods(appPod("web-a", "web", now, true), crashing, pending, other)
	fake.metrics = map[string]podUsage{"web-a": {cpuMillis: 142, memoryBytes: 214 << 20}}
	pe := newPodEnv(t, fake)

	var out podsJSON
	code, raw := pe.raw(t, "GET", "/api/v1/projects/shop/apps/web/pods", "")
	if code != http.StatusOK {
		t.Fatalf("pods: %d %s", code, raw)
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", raw)
	if len(out.Pods) != 3 || !out.Metrics || !out.Access.Logs || !out.Access.Exec {
		t.Fatalf("pods = %+v", out)
	}
	a, b, c := out.Pods[0], out.Pods[1], out.Pods[2]
	if a.Name != "web-a" || a.Node != "fsn1-node-1" || a.Status != "Running" || a.Tone != "ok" || !a.Ready ||
		a.CPUMillis == nil || *a.CPUMillis != 142 || *a.Memory != 214<<20 {
		t.Errorf("running pod = %+v", a)
	}
	if b.Status != "CrashLoopBackOff" || b.Tone != "bad" || b.Restarts != 4 || b.Ready || b.CPUMillis != nil ||
		b.Containers[0].LastTermination == nil || b.Containers[0].LastTermination.ExitCode != 1 || b.Message != "back-off 2m40s" {
		t.Errorf("crashing pod = %+v", b)
	}
	if c.Status != "Pending" || c.Tone != "warn" {
		t.Errorf("pending pod = %+v", c)
	}
	// The JSON keys the console reads.
	for _, key := range []string{`"cpuMillis":142`, `"memoryBytes":`, `"restarts":4`, `"lastTermination":`, `"access":{"logs":true,"exec":true}`} {
		if !strings.Contains(raw, key) {
			t.Errorf("response lacks %s", key)
		}
	}

	// No metrics-server: nulls, not zeros. A role without exec: false.
	fake.metrics = nil
	fake.denied["create/exec"] = true
	pe.raw(t, "GET", "/api/v1/projects/shop/apps/web/pods", "")
	_, raw = pe.raw(t, "GET", "/api/v1/projects/shop/apps/web/pods", "")
	if !strings.Contains(raw, `"metrics":false`) || !strings.Contains(raw, `"cpuMillis":null`) || !strings.Contains(raw, `"exec":false`) {
		t.Errorf("without metrics and exec: %s", raw)
	}
	if code, _ := pe.raw(t, "GET", "/api/v1/projects/shop/apps/missing/pods", ""); code != http.StatusNotFound {
		t.Errorf("pods of a missing app: %d, want 404", code)
	}
}

// ---- logs ------------------------------------------------------------------------

func TestLogsMergeReplicasInTimeOrderAndTagThem(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	fake := newFakePods(appPod("web-a", "web", base, true), appPod("web-b", "web", base, true))
	fake.backlog["web-a"] = []string{ts(base, 0) + " a1", ts(base, 20) + " a2", ts(base, 40) + " a3"}
	fake.backlog["web-b"] = []string{ts(base, 10) + " b1", ts(base, 30) + " b2"}
	pe := newPodEnv(t, fake)

	events, cancel, res := pe.stream(t, "/api/v1/projects/shop/apps/web/logs?tail=100&since=3600")
	defer cancel()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("logs: %d", res.StatusCode)
	}
	start := next(t, events, "start")
	if fmt.Sprint(start.data["pods"]) != "[web-a web-b]" || start.data["container"] != "app" {
		t.Errorf("start = %v", start.data)
	}
	var got []string
	for range 5 {
		ev := next(t, events, "line")
		got = append(got, fmt.Sprintf("%s:%s", ev.data["pod"], ev.data["text"]))
		if ev.data["ts"] == "" || ev.data["container"] != "app" {
			t.Errorf("line = %v", ev.data)
		}
	}
	if want := "[web-a:a1 web-b:b1 web-a:a2 web-b:b2 web-a:a3]"; fmt.Sprint(got) != want {
		t.Errorf("merged = %v, want %s", got, want)
	}
	if end := next(t, events, "end"); end.data["reason"] != "complete" {
		t.Errorf("end = %v", end.data)
	}
	for _, c := range fake.calls {
		if !c.Timestamps || c.TailLines == nil || *c.TailLines != 100 || c.SinceSeconds == nil || *c.SinceSeconds != 3600 || c.Container != "app" || c.LimitBytes == nil {
			t.Errorf("log options = %+v", c)
		}
	}
}

func TestLogsFollowResumesAfterTheBacklogWithoutRepeats(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	fake := newFakePods(appPod("web-a", "web", base, true), appPod("web-b", "web", base, true))
	fake.backlog["web-a"] = []string{ts(base, 0) + " old"}
	pe := newPodEnv(t, fake)

	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/apps/web/logs?follow=1")
	defer cancel()
	if ev := next(t, events, "line"); ev.data["text"] != "old" {
		t.Fatalf("backlog line = %v", ev.data)
	}
	// SinceTime has second precision, so Kubernetes sends "old" again: skipped.
	fake.write(t, "web-a", ts(base, 0)+" old")
	fake.write(t, "web-a", ts(base, 5000)+" new from a")
	fake.write(t, "web-b", ts(base, 6000)+" new from b")
	a, b := next(t, events, "line"), next(t, events, "line")
	got := []string{fmt.Sprint(a.data["pod"], ":", a.data["text"]), fmt.Sprint(b.data["pod"], ":", b.data["text"])}
	slices.Sort(got)
	if fmt.Sprint(got) != "[web-a:new from a web-b:new from b]" {
		t.Errorf("followed lines = %v", got)
	}
	for _, c := range fake.followCalls() {
		if c.SinceTime == nil || !c.Timestamps {
			t.Errorf("follow options = %+v", c)
		}
	}

	// A replica that appears later is picked up and streamed from its start.
	fake.mu.Lock()
	fake.pods = append(fake.pods, appPod("web-c", "web", time.Now(), true))
	fake.mu.Unlock()
	fake.write(t, "web-c", ts(time.Now(), 0)+" hello from c")
	if ev := next(t, events, "line"); ev.data["pod"] != "web-c" {
		t.Errorf("new replica line = %v", ev.data)
	}
}

func TestLogsRateLimitDropsAndReports(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.logs.rate, p.logs.burst = 1, 5 })
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/apps/web/logs?follow=1")
	defer cancel()
	next(t, events, "start")
	for i := range 50 {
		fake.write(t, "web-a", fmt.Sprintf("%s line %d", ts(time.Now(), 0), i))
	}
	lines, dropped := 0, 0
	timeout := time.After(3 * time.Second)
	for dropped == 0 {
		select {
		case ev := <-events:
			switch ev.name {
			case "line":
				lines++
			case "dropped":
				dropped = int(ev.data["lines"].(float64))
			}
		case <-timeout:
			t.Fatalf("no dropped event; %d lines", lines)
		}
	}
	if lines > 6 || lines+dropped < 45 {
		t.Errorf("%d lines sent, %d dropped of 50 (burst 5, 1/s)", lines, dropped)
	}
}

func TestLogsCutLongLines(t *testing.T) {
	base := time.Now()
	fake := newFakePods(appPod("web-a", "web", base, true))
	fake.backlog["web-a"] = []string{ts(base, 0) + " " + strings.Repeat("z", 1000)}
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.logs.maxLine = 100 })
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/apps/web/logs")
	defer cancel()
	ev := next(t, events, "line")
	if text := ev.data["text"].(string); len(text) > 100 || ev.data["truncated"] != true {
		t.Errorf("long line: %d bytes, truncated %v", len(text), ev.data["truncated"])
	}
}

func TestLogsStopWhenTheClientGoesAway(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true), appPod("web-b", "web", time.Now(), true))
	pe := newPodEnv(t, fake)
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/apps/web/logs?follow=1")
	next(t, events, "start")
	fake.write(t, "web-a", ts(time.Now(), 0)+" hi")
	fake.write(t, "web-b", ts(time.Now(), 0)+" hi")
	next(t, events, "line")

	cancel() // the browser closes the tab
	pe.waitIdle(t)
	if o, c := fake.opened.Load(), fake.closed.Load(); o == 0 || o != c {
		t.Errorf("%d log requests opened, %d closed", o, c)
	}
	// The slot is free again: the user's limit is not used up by dead streams.
	for range 6 {
		release := pe.pods.streams.acquire(owner["email"])
		if release == nil {
			t.Fatal("stream slots were not released")
		}
		defer release()
	}
}

func TestLogsEndWhenIdleOrTooLong(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.logs.idle, p.logs.maxDuration = 200*time.Millisecond, time.Hour })
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/apps/web/logs?follow=1")
	defer cancel()
	if end := next(t, events, "end"); end.data["reason"] != "idle" {
		t.Errorf("end = %v, want idle", end.data)
	}

	pe2 := newPodEnv(t, newFakePods(appPod("web-a", "web", time.Now(), true)), func(p *podsAPI) {
		p.logs.idle, p.logs.maxDuration = time.Hour, 300*time.Millisecond
	})
	events, cancel2, _ := pe2.stream(t, "/api/v1/projects/shop/apps/web/logs?follow=1")
	defer cancel2()
	if end := next(t, events, "end"); end.data["reason"] != "maxDuration" {
		t.Errorf("end = %v, want maxDuration", end.data)
	}
}

func TestLogsEndWhenTheSessionEnds(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.logs.sessionCheck = 50 * time.Millisecond })
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/apps/web/logs?follow=1")
	defer cancel()
	next(t, events, "start")
	pe.call(t, "DELETE", "/api/v1/session", nil, nil) // sign out in another tab
	if end := next(t, events, "end"); end.data["reason"] != "signedOut" {
		t.Errorf("end = %v, want signedOut", end.data)
	}
}

func TestLogsRequestChecks(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	pe := newPodEnv(t, fake)
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/api/v1/projects/shop/apps/web/logs?previous=1", http.StatusUnprocessableEntity},
		{"/api/v1/projects/shop/apps/web/logs?tail=-1", http.StatusUnprocessableEntity},
		{"/api/v1/projects/shop/apps/web/logs?pod=api-a", http.StatusNotFound},
		{"/api/v1/projects/shop/apps/web/logs?container=sidecar", http.StatusUnprocessableEntity},
		{"/api/v1/projects/shop/apps/missing/logs", http.StatusNotFound},
	} {
		if code, body := pe.raw(t, "GET", tc.path, ""); code != tc.code {
			t.Errorf("GET %s: %d %s, want %d", tc.path, code, body, tc.code)
		}
	}
	// Kubernetes says no: 403 and an audit entry.
	fake.denied["get/log"] = true
	if code, _ := pe.raw(t, "GET", "/api/v1/projects/shop/apps/web/logs", ""); code != http.StatusForbidden {
		t.Errorf("denied logs: %d, want 403", code)
	}
	entries, _ := pe.store.RecentAudit(context.Background(), 5)
	if len(entries) == 0 || entries[0].Action != "app.logs.denied" || entries[0].Target != "shop/web" {
		t.Errorf("audit = %+v", entries)
	}
	// Other sites cannot read logs with the visitor's cookie.
	req, _ := http.NewRequest("GET", pe.srv.URL+"/api/v1/projects/shop/apps/web/logs", nil)
	req.Header.Set("Origin", "https://evil.example")
	if res, err := pe.client.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site logs: %v %v, want 403", res.StatusCode, err)
	}
}

func TestTaskLogsUseTheTaskLabel(t *testing.T) {
	base := time.Now()
	pod := appPod("nightly-x1", "", base, true)
	pod.Labels = map[string]string{controllers.LabelTask: "nightly"}
	fake := newFakePods(pod, appPod("web-a", "web", base, true))
	fake.backlog["nightly-x1"] = []string{ts(base, 0) + " task output"}
	fake.backlog["web-a"] = []string{ts(base, 0) + " app output"}
	pe := newPodEnv(t, fake)
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/tasks/nightly/logs")
	defer cancel()
	if ev := next(t, events, "line"); ev.data["pod"] != "nightly-x1" || ev.data["text"] != "task output" {
		t.Errorf("task line = %v", ev.data)
	}
	if end := next(t, events, "end", "line"); end.name != "end" {
		t.Errorf("an app's line leaked into the task's logs: %v", end.data)
	}
	_, raw := pe.raw(t, "GET", "/api/v1/projects/shop/tasks/nightly/pods", "")
	if !strings.Contains(raw, `"name":"nightly-x1"`) || strings.Contains(raw, "web-a") {
		t.Errorf("task pods = %s", raw)
	}
}

func TestPreviousContainerLogs(t *testing.T) {
	base := time.Now()
	pod := appPod("web-a", "web", base, true)
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}
	fake := newFakePods(pod)
	fake.backlog["web-a"] = []string{ts(base, 0) + " panic: boom"}
	pe := newPodEnv(t, fake)
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/apps/web/logs?pod=web-a&previous=1&follow=1")
	defer cancel()
	next(t, events, "line")
	if end := next(t, events, "end"); end.data["reason"] != "complete" {
		t.Errorf("previous logs do not follow: %v", end.data)
	}
	if len(fake.calls) != 1 || !fake.calls[0].Previous || fake.calls[0].Follow {
		t.Errorf("calls = %+v", fake.calls)
	}
}

// ---- shells ----------------------------------------------------------------------

func wsURL(pe *podEnv, path string) string {
	return "ws" + strings.TrimPrefix(pe.srv.URL, "http") + path
}

func (pe *podEnv) dial(t *testing.T, path string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	d := websocket.Dialer{Jar: pe.client.Jar, HandshakeTimeout: 5 * time.Second}
	return d.Dial(wsURL(pe, path), hdr)
}

func originOf(pe *podEnv) http.Header { return http.Header{"Origin": {pe.srv.URL}} }

const shellPath = "/api/v1/projects/shop/apps/web/pods/web-a/shell"

func TestShellHandshakeRequiresSessionAndSameOrigin(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	fake.exec = func(ctx context.Context, _ *corev1.PodExecOptions, _ execStreams) error { <-ctx.Done(); return nil }
	pe := newPodEnv(t, fake)

	// No session cookie.
	anon := pe.newClient()
	anonPE := &podEnv{env: anon, fake: fake, pods: pe.pods}
	if _, res, err := anonPE.dial(t, shellPath, originOf(pe)); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %v %v, want 401", res, err)
	}
	// Another site's page, with the user's cookie.
	if _, res, err := pe.dial(t, shellPath, http.Header{"Origin": {"https://evil.example"}}); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin: %v %v, want 403", res, err)
	}
	// Same host on a look-alike origin.
	u, _ := url.Parse(pe.srv.URL)
	if _, res, err := pe.dial(t, shellPath, http.Header{"Origin": {"http://" + u.Hostname() + ".evil.example:" + u.Port()}}); err == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("look-alike origin: %v %v, want 403", res, err)
	}
	// No Origin at all: not a browser page; refused.
	if _, res, err := pe.dial(t, shellPath, nil); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("without Origin: %v %v, want 403", res, err)
	}
	// A plain GET is not a WebSocket.
	if code, _ := pe.raw(t, "GET", shellPath, ""); code != http.StatusBadRequest {
		t.Errorf("plain GET: %d, want 400", code)
	}
	if entries, _ := os.ReadDir(pe.dir); len(entries) != 0 {
		t.Errorf("refused handshakes left recordings: %v", entries)
	}
	// The console's own page gets through.
	conn, _, err := pe.dial(t, shellPath, originOf(pe))
	if err != nil {
		t.Fatalf("same origin: %v", err)
	}
	conn.Close()
}

type shellClient struct {
	t    *testing.T
	conn *websocket.Conn
}

// event reads until the next JSON event, collecting output on the way.
func (c *shellClient) event(out *bytes.Buffer) map[string]any {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			c.t.Fatalf("read: %v (output so far %q)", err, out)
		}
		if kind == websocket.BinaryMessage {
			out.Write(data)
			continue
		}
		var ev map[string]any
		_ = json.Unmarshal(data, &ev)
		return ev
	}
}

func (c *shellClient) waitOutput(out *bytes.Buffer, want string) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !strings.Contains(out.String(), want) {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			c.t.Fatalf("waiting for %q: %v (got %q)", want, err, out)
		}
		if kind == websocket.BinaryMessage {
			out.Write(data)
		}
	}
}

// echoShell upper-cases input until it reads "exit", then exits with 3.
func echoShell(ctx context.Context, opts *corev1.PodExecOptions, s execStreams) error {
	go func() {
		for size := s.resize.Next(); size != nil; size = s.resize.Next() {
			fmt.Fprintf(s.stdout, "[%dx%d]", size.Width, size.Height)
		}
	}()
	fmt.Fprintf(s.stdout, "$ %s ", strings.Join(opts.Command[:1], ""))
	buf := make([]byte, 1024)
	for {
		n, err := s.stdin.Read(buf)
		if err != nil {
			return nil
		}
		in := string(buf[:n])
		if strings.Contains(in, "exit") {
			return utilexec.CodeExitError{Err: fmt.Errorf("exit"), Code: 3}
		}
		fmt.Fprint(s.stdout, strings.ToUpper(in))
	}
}

func TestShellSessionIsAuditedAndRecorded(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	fake.exec = echoShell
	pe := newPodEnv(t, fake)

	conn, _, err := pe.dial(t, shellPath+"?shell=bash&cols=100&rows=30", originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	started := c.event(&out)
	if started["type"] != "started" || started["container"] != "app" || started["shell"] != "bash" || started["recording"] == "" {
		t.Fatalf("started = %v", started)
	}
	c.waitOutput(&out, "[100x30]")
	pe.clock.Advance(1500 * time.Millisecond)
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("echo grüße\n")) // ü and ß are two bytes each
	c.waitOutput(&out, "ECHO GRÜßE")                                       // strings.ToUpper keeps ß
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":120,"rows":40}`))
	c.waitOutput(&out, "[120x40]")
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("s3cret-typed-at-a-prompt exit\n"))
	exit := c.event(&out)
	if exit["type"] != "exit" || exit["reason"] != "exited" || exit["code"] != float64(3) {
		t.Errorf("exit = %v", exit)
	}
	pe.waitIdle(t)

	id := started["recording"].(string)
	cast, err := os.ReadFile(filepath.Join(pe.dir, id+".cast"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(cast)), "\n")
	var header castHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header.Version != 2 || header.Width != 100 || header.Height != 30 || header.Timestamp == 0 {
		t.Errorf("header = %s (%v)", lines[0], err)
	}
	var output strings.Builder
	codes := map[string]int{}
	for _, l := range lines[1:] {
		var ev []any
		if err := json.Unmarshal([]byte(l), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("event %q: %v", l, err)
		}
		if _, ok := ev[0].(float64); !ok {
			t.Errorf("event time %v is not a number", ev[0])
		}
		code, data := ev[1].(string), ev[2].(string)
		codes[code]++
		if code == "o" {
			output.WriteString(data)
		}
		if code == "r" && data != "120x40" {
			t.Errorf("resize event %q", data)
		}
	}
	if !strings.Contains(output.String(), "ECHO GRÜßE") || codes["r"] != 1 || codes["i"] != 0 {
		t.Errorf("recorded %v, output %q", codes, output.String())
	}
	if strings.Contains(string(cast), "s3cret") {
		t.Error("input that was not echoed ended up in the recording")
	}
	if !strings.Contains(string(cast), "1.5") {
		t.Errorf("event times do not follow the clock: %s", cast)
	}
	meta, _ := os.ReadFile(filepath.Join(pe.dir, id+".json"))
	var m recordingMeta
	if err := json.Unmarshal(meta, &m); err != nil || m.Ended == nil || m.Reason != "exited" || m.ExitCode == nil || *m.ExitCode != 3 ||
		m.User != owner["email"] || m.Pod != "web-a" || m.Container != "app" || m.Input {
		t.Errorf("sidecar = %s (%v)", meta, err)
	}
	if info, _ := os.Stat(filepath.Join(pe.dir, id+".cast")); info.Mode().Perm() != 0o600 {
		t.Errorf("recording mode %v, want 0600", info.Mode().Perm())
	}

	entries, _ := pe.store.RecentAudit(context.Background(), 10)
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action+" "+e.Target+" "+e.Detail)
	}
	if !slices.ContainsFunc(actions, func(s string) bool {
		return strings.HasPrefix(s, "pod.exec shop/web-a container app, bash, recording "+id)
	}) ||
		!slices.ContainsFunc(actions, func(s string) bool {
			return strings.HasPrefix(s, "pod.exec.end shop/web-a exited after") && strings.HasSuffix(s, "exit code 3")
		}) {
		t.Errorf("audit = %v", actions)
	}
}

func TestShellEndsWhenIdleOrTooLong(t *testing.T) {
	for _, tc := range []struct {
		name        string
		idle, max   time.Duration
		typeEvery   time.Duration
		wantReason  string
		wantMessage string
	}{
		{"idle", 200 * time.Millisecond, time.Hour, 0, "idle", "without input"},
		{"max length", 300 * time.Millisecond, 600 * time.Millisecond, 50 * time.Millisecond, "maxDuration", "end after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakePods(appPod("web-a", "web", time.Now(), true))
			var stopped atomic.Bool
			fake.exec = func(ctx context.Context, _ *corev1.PodExecOptions, s execStreams) error {
				go func() { _, _ = io.Copy(io.Discard, s.stdin) }()
				<-ctx.Done()
				stopped.Store(true)
				return ctx.Err()
			}
			pe := newPodEnv(t, fake, func(p *podsAPI) { p.shell.idle, p.shell.maxDuration = tc.idle, tc.max })
			conn, _, err := pe.dial(t, shellPath, originOf(pe))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if tc.typeEvery > 0 { // keep typing: idle never fires
				done := make(chan struct{})
				defer close(done)
				go func() {
					for {
						select {
						case <-done:
							return
						case <-time.After(tc.typeEvery):
							_ = conn.WriteMessage(websocket.BinaryMessage, []byte("x"))
						}
					}
				}()
			}
			c := &shellClient{t: t, conn: conn}
			var out bytes.Buffer
			c.event(&out) // started
			exit := c.event(&out)
			if exit["type"] != "exit" || exit["reason"] != tc.wantReason || !strings.Contains(exit["message"].(string), tc.wantMessage) {
				t.Errorf("exit = %v", exit)
			}
			pe.waitIdle(t)
			if !stopped.Load() {
				t.Error("exec kept running after the session ended")
			}
		})
	}
}

func TestShellRefusedWhenKubernetesSaysNo(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true), appPod("api-a", "api", time.Now(), true), appPod("web-p", "web", time.Now(), false))
	fake.exec = func(context.Context, *corev1.PodExecOptions, execStreams) error {
		t.Error("exec ran")
		return nil
	}
	pe := newPodEnv(t, fake)
	viewer := &podEnv{env: pe.asRole(t, store.RoleViewer), pods: pe.pods}
	fake.denied["create/exec"] = true

	for _, tc := range []struct {
		who  *podEnv
		path string
		want string
	}{
		{viewer, shellPath, "Your role does not allow opening a shell."},
		{pe, "/api/v1/projects/shop/apps/web/pods/api-a/shell", `Replica "api-a" is not part of app "web".`},
		{pe, "/api/v1/projects/shop/apps/web/pods/web-p/shell", "Cannot open a shell in app: it has not started yet."},
		{pe, "/api/v1/projects/shop/apps/web/pods/gone/shell", `Replica "gone" not found.`},
		{pe, "/api/v1/projects/shop/apps/web/pods/web-a/shell?container=nope", `There is no container "nope" in web-a.`},
	} {
		conn, _, err := tc.who.dial(t, tc.path, originOf(pe))
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		c := &shellClient{t: t, conn: conn}
		var out bytes.Buffer
		if ev := c.event(&out); ev["type"] != "error" || !strings.HasPrefix(ev["message"].(string), tc.want) {
			t.Errorf("%s: %v, want error %q", tc.path, ev, tc.want)
		}
		conn.Close()
	}
	pe.waitIdle(t)
	entries, _ := pe.store.RecentAudit(context.Background(), 10)
	if !slices.ContainsFunc(entries, func(e store.AuditEntry) bool {
		return e.Action == "pod.exec.denied" && e.Actor == "viewer@example.com" && e.Target == "shop/web-a"
	}) {
		t.Errorf("audit lacks the denied shell: %+v", entries)
	}
	if files, _ := os.ReadDir(pe.dir); len(files) != 0 {
		t.Errorf("refused sessions left recordings: %v", files)
	}
	if code, _ := pe.raw(t, "GET", shellPath+"?shell=zsh", ""); code != http.StatusBadRequest {
		// plain GET is refused before the shell is looked at
		t.Errorf("plain GET with a bad shell: %d", code)
	}
}

func TestShellEndsAtTheRecordingLimit(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	fake.exec = func(ctx context.Context, _ *corev1.PodExecOptions, s execStreams) error {
		chunk := []byte(strings.Repeat("A", 1000))
		for ctx.Err() == nil {
			if _, err := s.stdout.Write(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.rec.maxBytes = 10 << 10 })
	conn, _, err := pe.dial(t, shellPath, originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	c.event(&out)
	if exit := c.event(&out); exit["reason"] != "recordingFull" {
		t.Errorf("exit = %v", exit)
	}
	if out.Len() > 10<<10 {
		t.Errorf("%d bytes shown, more than was recorded", out.Len())
	}
	pe.waitIdle(t)
	files, _ := filepath.Glob(filepath.Join(pe.dir, "*.cast"))
	if len(files) != 1 {
		t.Fatalf("recordings: %v", files)
	}
	info, _ := os.Stat(files[0])
	if info.Size() > 11<<10 {
		t.Errorf("recording is %d bytes, over the limit", info.Size())
	}
}

func TestShellsNeedARecordingsDirectory(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.rec = nil })
	if _, res, err := pe.dial(t, shellPath, originOf(pe)); err == nil || res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("without recordings: %v %v, want 503", res, err)
	}
}

// ---- recordings ----------------------------------------------------------------

func TestRecordingKeepsSplitCharactersTogether(t *testing.T) {
	dir := t.TempDir()
	rc := &recorder{dir: dir, retention: time.Hour, maxBytes: 1 << 20}
	start := time.Unix(1_790_000_000, 0)
	rec, err := rc.start(recordingMeta{User: "a@example.com", Project: "p", Pod: "x", Container: "app", Shell: "sh", Started: start}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	euro := []byte("€") // three bytes
	_ = rec.output(append([]byte("price: "), euro[:1]...), start.Add(time.Second))
	_ = rec.output(euro[1:], start.Add(2*time.Second))
	_ = rec.output([]byte{0xff, '!'}, start.Add(3*time.Second)) // invalid UTF-8
	_ = rec.output(euro[:2], start.Add(4*time.Second))          // cut off at the end
	if err := rec.finish(start.Add(5*time.Second), "exited", nil); err != nil {
		t.Fatal(err)
	}
	cast, _ := os.ReadFile(filepath.Join(dir, rec.meta.ID+".cast"))
	lines := strings.Split(strings.TrimSpace(string(cast)), "\n")
	want := []string{
		`[1.000000, "o", "price: "]`,
		`[2.000000, "o", "€"]`,
		`[3.000000, "o", "�!"]`,
		`[5.000000, "o", "��"]`,
	}
	if len(lines) != 5 || fmt.Sprint(lines[1:]) != fmt.Sprint(want) {
		t.Errorf("events:\n%s\nwant\n%s", strings.Join(lines[1:], "\n"), strings.Join(want, "\n"))
	}
	if !recordingIDRE.MatchString(rec.meta.ID) {
		t.Errorf("id %q", rec.meta.ID)
	}
}

func TestRecordingsAreForOwnersAndAdmins(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	fake.exec = func(context.Context, *corev1.PodExecOptions, execStreams) error { return nil }
	pe := newPodEnv(t, fake)
	conn, _, err := pe.dial(t, shellPath, originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	id := c.event(&out)["recording"].(string)
	c.event(&out) // exit
	conn.Close()
	pe.waitIdle(t)

	var list []recordingMeta
	code, raw := pe.raw(t, "GET", "/api/v1/recordings", "")
	if err := json.Unmarshal([]byte(raw), &list); code != http.StatusOK || err != nil || len(list) != 1 || list[0].ID != id || list[0].Reason != "exited" {
		t.Fatalf("list: %d %s", code, raw)
	}
	code, cast := pe.raw(t, "GET", "/api/v1/recordings/"+id, "")
	if code != http.StatusOK || !strings.HasPrefix(cast, `{"version":2,`) {
		t.Errorf("download: %d %q", code, cast)
	}
	for _, bad := range []string{"..%2F..%2Fkwerft.db", "20261004T091402Z-zzzzzzzz", "nope"} {
		if code, _ := pe.raw(t, "GET", "/api/v1/recordings/"+bad, ""); code != http.StatusNotFound {
			t.Errorf("download %q: %d, want 404", bad, code)
		}
	}
	entries, _ := pe.store.RecentAudit(context.Background(), 5)
	if !slices.ContainsFunc(entries, func(e store.AuditEntry) bool { return e.Action == "recording.download" && e.Target == id }) {
		t.Errorf("download not audited: %+v", entries)
	}
	for _, role := range []string{store.RoleDeveloper, store.RoleViewer} {
		other := pe.asRole(t, role)
		if code, _ := other.raw(t, "GET", "/api/v1/recordings", ""); code != http.StatusForbidden {
			t.Errorf("%s lists recordings: %d, want 403", role, code)
		}
		if code, _ := other.raw(t, "GET", "/api/v1/recordings/"+id, ""); code != http.StatusForbidden {
			t.Errorf("%s downloads a recording: %d, want 403", role, code)
		}
	}
}

func TestOldRecordingsArePruned(t *testing.T) {
	dir := t.TempDir()
	rc := &recorder{dir: dir, retention: 24 * time.Hour, maxBytes: 1 << 20, log: slog.New(slog.DiscardHandler)}
	now := time.Now()
	old, _ := rc.start(recordingMeta{User: "a", Started: now.Add(-48 * time.Hour)}, 80, 24)
	_ = old.finish(now.Add(-47*time.Hour), "exited", nil)
	past := now.Add(-47 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, old.meta.ID+".cast"), past, past)
	rc.lastPrune = time.Time{}
	fresh, _ := rc.start(recordingMeta{User: "b", Started: now}, 80, 24)
	_ = fresh.finish(now, "exited", nil)
	list, _ := rc.list(10, recordingFilter{})
	if len(list) != 1 || list[0].ID != fresh.meta.ID {
		t.Errorf("after pruning: %+v", list)
	}
}

func TestSlotsLimitConcurrentStreams(t *testing.T) {
	s := newSlots(2, 3)
	r1, r2 := s.acquire("a"), s.acquire("a")
	if r1 == nil || r2 == nil || s.acquire("a") != nil {
		t.Fatal("per-user limit")
	}
	if s.acquire("b") == nil || s.acquire("c") != nil {
		t.Fatal("total limit")
	}
	r1()
	r1() // twice is harmless
	if s.acquire("a") == nil {
		t.Error("released slot not reusable")
	}
}

func TestTokenBucket(t *testing.T) {
	b := newTokenBucket(10, 3)
	now := time.Now()
	n := 0
	for range 10 {
		if b.take(now) {
			n++
		}
	}
	if n != 3 {
		t.Errorf("burst allowed %d, want 3", n)
	}
	if !b.take(now.Add(150 * time.Millisecond)) {
		t.Error("no token after 150ms at 10/s")
	}
}
