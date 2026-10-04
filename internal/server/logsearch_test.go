package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/logs"
)

// fakeVictoriaLogs answers /select/logsql/query with body and keeps
// /select/logsql/tail open after writing tail. It records every request.
type fakeVictoriaLogs struct {
	mu     sync.Mutex
	srv    *httptest.Server
	forms  []url.Values
	paths  []string
	status int
	body   string
	tail   string
}

func newFakeVictoriaLogs(t *testing.T) *fakeVictoriaLogs {
	f := &fakeVictoriaLogs{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.forms = append(f.forms, r.PostForm)
		f.paths = append(f.paths, r.URL.Path)
		status, body, tail := f.status, f.body, f.tail
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		if r.URL.Path == "/select/logsql/tail" {
			_, _ = io.WriteString(w, tail)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVictoriaLogs) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeVictoriaLogs) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forms)
}

// last is the newest request's form.
func (f *fakeVictoriaLogs) last(t *testing.T) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forms) == 0 {
		t.Fatal("VictoriaLogs got no request")
	}
	return f.forms[len(f.forms)-1]
}

// confinement decodes the first extra_filters arg and checks that
// extra_stream_filters says the same.
func (f *fakeVictoriaLogs) confinement(t *testing.T) map[string]any {
	t.Helper()
	form := f.last(t)
	if form.Get("extra_filters") == "" || form.Get("extra_filters") != form.Get("extra_stream_filters") {
		t.Fatalf("extra_filters %q, extra_stream_filters %q", form["extra_filters"], form["extra_stream_filters"])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(form.Get("extra_filters")), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func namespacesOf(m map[string]any) []string {
	var out []string
	for _, v := range m["namespace"].([]any) {
		out = append(out, v.(string))
	}
	return out
}

func vlLine(at time.Time, ns, pod, msg string, extra map[string]string) string {
	rec := map[string]string{"_time": at.UTC().Format(time.RFC3339Nano), "_msg": msg, "namespace": ns, "pod": pod, "container": "app", "stream": "stdout"}
	for k, v := range extra {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	return string(b) + "\n"
}

// readEvents reads Server-Sent Events from an open response.
func readEvents(res *http.Response) <-chan sse {
	events := make(chan sse, 100)
	go func() {
		defer close(events)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
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
	return events
}

type searchResult struct {
	Entries []struct {
		Time      time.Time `json:"time"`
		Namespace string    `json:"namespace"`
		Project   string    `json:"project"`
		App       string    `json:"app"`
		Pod       string    `json:"pod"`
		Line      string    `json:"line"`
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

func TestLogSearchIsConfinedToTheUsersProjects(t *testing.T) {
	vl := newFakeVictoriaLogs(t)
	var search *logSearchAPI
	c := newConsole(t, func(cfg *Config) {
		cfg.LogsURL = vl.srv.URL
		cfg.logsHook = func(s *logSearchAPI) { search = s }
	})
	c.project(t, "logs-a")
	c.project(t, "logs-b")
	if code := c.dev.do(t, "POST", "/api/v1/projects/logs-a/apps", map[string]any{"name": "web", "spec": map[string]any{"source": map[string]any{"image": map[string]any{"ref": "nginx:1.27"}}}}, nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	// A namespace that claims to be a project it is not, and a platform
	// namespace: neither may ever be in a user's scope.
	ctx := context.Background()
	for _, ns := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "logs-squat", Labels: map[string]string{controllers.LabelProject: "logs-a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "kwerft-builds-logs-test", Labels: map[string]string{controllers.LabelProject: "kwerft-builds-logs-test"}}},
	} {
		if err := cluster.admin.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), ns) })
	}

	now := time.Now()
	vl.set(0, vlLine(now.Add(-time.Second), "logs-a", "web-1", "GET /healthz 200", map[string]string{"app": "web", "project": "logs-a"})+
		vlLine(now.Add(-2*time.Second), "logs-b", "debug", "made with kubectl run", nil))

	// Whatever the query says, the scope is the user's projects.
	var res searchResult
	q := url.Values{"query": {`namespace:kube-system OR * OR {namespace="kwerft-builds"}`}}
	if code := c.dev.do(t, "GET", "/api/v1/logs?"+q.Encode(), nil, &res); code != http.StatusOK {
		t.Fatalf("search: %d", code)
	}
	ns := namespacesOf(vl.confinement(t))
	if !slices.Contains(ns, "logs-a") || !slices.Contains(ns, "logs-b") {
		t.Errorf("scope %v lacks the projects", ns)
	}
	for _, n := range ns {
		if strings.HasPrefix(n, "kube-") || strings.HasPrefix(n, "kwerft-") || n == "default" || n == "logs-squat" {
			t.Errorf("scope %v contains %s", ns, n)
		}
	}
	if got := vl.last(t).Get("query"); !strings.HasPrefix(got, q.Get("query")+"\n| fields ") {
		t.Errorf("query = %q", got)
	}
	// Oldest first; lines from project namespaces without the label get the
	// project from their namespace.
	if len(res.Entries) != 2 || res.Entries[0].Line != "made with kubectl run" || res.Entries[0].Project != "logs-b" ||
		res.Entries[1].App != "web" || res.Entries[1].Project != "logs-a" {
		t.Errorf("entries = %+v", res.Entries)
	}

	// One project, one app: the user's own get of the App decides.
	if code := c.viewer.do(t, "GET", "/api/v1/logs?project=logs-a&app=web&level=error&since=15m&limit=50", nil, &res); code != http.StatusOK {
		t.Fatalf("app search: %d", code)
	}
	if conf := vl.confinement(t); !slices.Equal(namespacesOf(conf), []string{"logs-a"}) || conf["app"] != "web" {
		t.Errorf("app scope = %v", conf)
	}
	if form := vl.last(t); len(form["extra_filters"]) != 2 || !strings.Contains(form.Get("query"), "limit 51") {
		t.Errorf("level filter or limit missing: %v", form)
	}

	before := vl.requests()
	var e apiError
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/api/v1/logs?project=logs-squat", http.StatusNotFound},
		{"/api/v1/logs?project=kube-system", http.StatusNotFound},
		{"/api/v1/logs?project=logs-a&app=nope", http.StatusNotFound},
		{"/api/v1/logs?app=web", http.StatusUnprocessableEntity},
		{"/api/v1/logs?query=" + url.QueryEscape("* | stats count() by (namespace)"), http.StatusUnprocessableEntity},
		{"/api/v1/logs?query=" + url.QueryEscape(`"open`), http.StatusUnprocessableEntity},
		{"/api/v1/logs?level=debug", http.StatusUnprocessableEntity},
		{"/api/v1/logs?since=1h&until=2h", http.StatusUnprocessableEntity},
		{"/api/v1/logs?since=2000h", http.StatusUnprocessableEntity},
		{"/api/v1/logs?platform=1", http.StatusForbidden},
		{"/api/v1/logs/tail?platform=1", http.StatusForbidden},
	} {
		if code := c.dev.do(t, "GET", tc.path, nil, &e); code != tc.code {
			t.Errorf("%s: %d %+v, want %d", tc.path, code, e, tc.code)
		}
	}
	if n := vl.requests(); n != before {
		t.Errorf("%d refused requests reached VictoriaLogs", n-before)
	}

	// Owners may search the platform too.
	if code := c.owner.do(t, "GET", "/api/v1/logs?platform=1", nil, &res); code != http.StatusOK {
		t.Fatalf("platform search: %d", code)
	}
	if ns := namespacesOf(vl.confinement(t)); !slices.Contains(ns, "kube-system") || !slices.Contains(ns, "logs-a") {
		t.Errorf("platform scope = %v", ns)
	}

	// VictoriaLogs down: a clear 503.
	vl.set(http.StatusServiceUnavailable, "")
	if code := c.dev.do(t, "GET", "/api/v1/logs", nil, &e); code != http.StatusServiceUnavailable || !strings.Contains(e.Error, "VictoriaLogs") {
		t.Errorf("down: %d %+v", code, e)
	}
	vl.set(http.StatusBadRequest, "cannot parse query [x]: unexpected token")
	if code := c.dev.do(t, "GET", "/api/v1/logs?query=x", nil, &e); code != http.StatusUnprocessableEntity || !strings.Contains(e.Error, "unexpected token") {
		t.Errorf("bad query: %d %+v", code, e)
	}
	vl.set(0, "")

	// Live tail: the same scope, as Server-Sent Events.
	vl.mu.Lock()
	vl.tail = vlLine(now, "logs-a", "web-1", "\x1b[32mready\x1b[0m", map[string]string{"app": "web"})
	vl.mu.Unlock()
	tctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(tctx, "GET", c.dev.url+"/api/v1/logs/tail?project=logs-a&app=web&query=ready", nil)
	resp, err := c.dev.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("tail: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readEvents(resp)
	next(t, events, "start")
	if ln := next(t, events, "line"); ln.data["line"] != "\x1b[32mready\x1b[0m" || ln.data["project"] != "logs-a" || ln.data["pod"] != "web-1" {
		t.Errorf("tail line = %v", ln.data)
	}
	if conf := vl.confinement(t); !slices.Equal(namespacesOf(conf), []string{"logs-a"}) || conf["app"] != "web" || vl.paths[len(vl.paths)-1] != "/select/logsql/tail" {
		t.Errorf("tail scope = %v", conf)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for search.running.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("tail still running after the client left")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- history of Task and Build logs ------------------------------------------------

func TestBuildLogFromHistoryWhenThePodIsGone(t *testing.T) {
	vl := newFakeVictoriaLogs(t)
	created := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	finished := metav1.NewTime(created.Add(3 * time.Minute))
	fb := &fakeBuilds{builds: map[string]*kwerftv1.Build{
		"shop/pruned": {ObjectMeta: metav1.ObjectMeta{Name: "pruned", Namespace: "shop", CreationTimestamp: created},
			Status: kwerftv1.BuildStatus{Phase: kwerftv1.BuildFailed, CompletionTime: &finished}},
		"shop/forgotten": {ObjectMeta: metav1.ObjectMeta{Name: "forgotten", Namespace: "shop"}, Status: kwerftv1.BuildStatus{Phase: kwerftv1.BuildFailed}},
	}}
	fake := newFakePods()
	var api *buildsAPI
	e := newEnv(t, func(c *Config) {
		c.LogsURL = vl.srv.URL
		c.buildsHook = func(b *buildsAPI) {
			b.getBuild = fb.get
			b.ownPods = func() (podBackend, error) { return fakeBackend{fake}, nil }
			b.logs = fastLogLimits()
			api = b
		}
	})
	e.completeSetup(t)
	pe := &podEnv{env: e, fake: fake}
	t.Cleanup(func() {
		for api.running.Load() > 0 {
			time.Sleep(10 * time.Millisecond)
		}
	})

	at := created.Add(time.Minute)
	vl.set(0, vlLine(at.Add(time.Second), "kwerft-builds", "pruned-job-x", "ERROR: failed to solve", map[string]string{"build": "pruned", "container": "build"})+
		vlLine(at, "kwerft-builds", "pruned-job-x", "Cloning into '/workspace'...", map[string]string{"build": "pruned", "container": "clone"}))
	events, cancel, res := pe.stream(t, "/api/v1/projects/shop/builds/pruned/logs?follow=1&tail=100")
	defer cancel()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", res.StatusCode)
	}
	start := next(t, events, "start")
	if start.data["source"] != "history" || !strings.Contains(start.data["message"].(string), "log history") {
		t.Errorf("start = %v", start.data)
	}
	if ln := next(t, events, "line"); ln.data["text"] != "Cloning into '/workspace'..." || ln.data["container"] != "clone" || ln.data["pod"] != "pruned-job-x" {
		t.Errorf("first line = %v", ln.data)
	}
	if ln := next(t, events, "line"); ln.data["text"] != "ERROR: failed to solve" {
		t.Errorf("second line = %v", ln.data)
	}
	if end := next(t, events, "end"); end.data["reason"] != "complete" {
		t.Errorf("end = %v", end.data)
	}
	// Confined to this Build's pods in kwerft-builds, over its lifetime.
	if conf := vl.confinement(t); !slices.Equal(namespacesOf(conf), []string{"kwerft-builds"}) || conf["build"] != "pruned" || conf["project"] != "shop" || len(conf) != 3 {
		t.Errorf("history scope = %v", conf)
	}
	form := vl.last(t)
	if form.Get("start") != created.Add(-time.Minute).UTC().Format(time.RFC3339Nano) || form.Get("end") != finished.Add(10*time.Minute).UTC().Format(time.RFC3339Nano) ||
		!strings.Contains(form.Get("query"), "limit 101") || !strings.HasPrefix(form.Get("query"), "*\n") {
		t.Errorf("history query = %v", form)
	}

	// Nothing in the history either: "gone", as before.
	vl.set(0, "")
	events, cancel2, _ := pe.stream(t, "/api/v1/projects/shop/builds/forgotten/logs")
	defer cancel2()
	if end := next(t, events, "end"); end.data["reason"] != "gone" {
		t.Errorf("end = %v", end.data)
	}
	// VictoriaLogs down: "gone" too.
	vl.set(http.StatusServiceUnavailable, "")
	events, cancel3, _ := pe.stream(t, "/api/v1/projects/shop/builds/pruned/logs")
	defer cancel3()
	if end := next(t, events, "end"); end.data["reason"] != "gone" {
		t.Errorf("end = %v", end.data)
	}
}

func TestTaskLogFromHistoryWhenThePodsAreGone(t *testing.T) {
	vl := newFakeVictoriaLogs(t)
	base := time.Now().Add(-time.Hour)
	fake := newFakePods(appPod("live-abcde", "", base, true))
	fake.pods[0].Labels = map[string]string{controllers.LabelTask: "live"}
	fake.backlog["live-abcde"] = []string{ts(base, 0) + " from the pod"}
	fake.taskPhases = map[string]kwerftv1.TaskPhase{"done": kwerftv1.TaskSucceeded, "live": kwerftv1.TaskSucceeded, "waiting": kwerftv1.TaskPending}
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.vlogs = logs.New(vl.srv.URL) })

	vl.set(0, vlLine(base, "shop", "done-xyz", "migrated 12 tables", map[string]string{"task": "done", "container": "task"}))
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/tasks/done/logs?follow=1")
	defer cancel()
	if start := next(t, events, "start"); start.data["source"] != "history" {
		t.Errorf("start = %v", start.data)
	}
	if ln := next(t, events, "line"); ln.data["text"] != "migrated 12 tables" || ln.data["pod"] != "done-xyz" {
		t.Errorf("line = %v", ln.data)
	}
	next(t, events, "end")
	if conf := vl.confinement(t); !slices.Equal(namespacesOf(conf), []string{"shop"}) || conf["task"] != "done" || len(conf) != 2 {
		t.Errorf("history scope = %v", conf)
	}

	// The pod still there: the live log, as before, without asking VictoriaLogs.
	before := vl.requests()
	events, cancel2, _ := pe.stream(t, "/api/v1/projects/shop/tasks/live/logs")
	defer cancel2()
	if start := next(t, events, "start"); start.data["source"] != nil {
		t.Errorf("live start = %v", start.data)
	}
	if ln := next(t, events, "line"); ln.data["text"] != "from the pod" {
		t.Errorf("live line = %v", ln.data)
	}
	// An unfinished Task without pods waits for them, as before.
	events, cancel3, _ := pe.stream(t, "/api/v1/projects/shop/tasks/waiting/logs")
	defer cancel3()
	next(t, events, "end")
	if n := vl.requests(); n != before {
		t.Errorf("VictoriaLogs asked %d times for live logs", n-before)
	}
	// The user's RBAC still applies first.
	fake.mu.Lock()
	fake.denied["get/log"] = true
	fake.mu.Unlock()
	_, cancel4, res := pe.stream(t, "/api/v1/projects/shop/tasks/done/logs")
	cancel4()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("denied: %d", res.StatusCode)
	}
}
