// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/metrics"
)

// fakeVM stands in for VictoriaMetrics: it records every request and
// answers each query with one series.
type fakeVM struct {
	mu   sync.Mutex
	reqs []vmRequest
	down bool
	ns   string // the namespace in answers
}

type vmRequest struct {
	path    string
	filters []string // extra_filters[] from the URL
	form    url.Values
}

func (f *fakeVM) start(t *testing.T) *metrics.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		f.reqs = append(f.reqs, vmRequest{path: r.URL.Path, filters: r.URL.Query()["extra_filters[]"], form: form})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/query_range":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"`+f.ns+`"},"values":[[1700000000,"1"],[1700000030,"2"]]}]}}`)
		case strings.Contains(form.Get("query"), "nodename"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"nodename":"node-1"},"value":[1700000000,"4"]}]}}`)
		case r.URL.Path == "/api/v1/query":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"`+f.ns+`","app":"web"},"value":[1700000000,"0.25"]}]}}`)
		default:
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"namespace":"`+f.ns+`"},"values":[[1700000000,"1"],[1700000030,"2"]]}]}}`)
		}
	}))
	t.Cleanup(srv.Close)
	if f.down {
		srv.Close()
	}
	return metrics.New(srv.URL)
}

func (f *fakeVM) requests() []vmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vmRequest(nil), f.reqs...)
}

func (f *fakeVM) reset() {
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
}

// metricsConsole is a console with a fake VictoriaMetrics and a project
// with an App "web"; the project is removed afterwards.
func metricsConsole(t *testing.T, vm *fakeVM, project string) *console {
	t.Helper()
	requireCluster(t)
	vm.ns = project
	client := vm.start(t)
	c := newConsole(t, func(cfg *Config) { cfg.Metrics = client })
	c.project(t, project)
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: project}})
	})
	if code := c.dev.do(t, "POST", "/api/v1/projects/"+project+"/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	return c
}

// confined checks that a request carries exactly one extra_filters[], a
// namespace regex that lists project namespaces only (the shared test
// cluster has other tests' projects too), including want.
func confined(t *testing.T, r vmRequest, want string) {
	t.Helper()
	if len(r.filters) != 1 {
		t.Fatalf("%s %q: extra_filters[] = %q, want exactly one", r.path, r.form.Get("query"), r.filters)
	}
	m := regexp.MustCompile(`^\{namespace=~"([a-z0-9|-]+)"\}$`).FindStringSubmatch(r.filters[0])
	if m == nil {
		t.Fatalf("filter %q is not a namespace regex", r.filters[0])
	}
	found := false
	for _, ns := range strings.Split(m[1], "|") {
		if metrics.IsPlatformNamespace(ns) {
			t.Errorf("filter %q lets platform namespace %s through", r.filters[0], ns)
		}
		found = found || ns == want
	}
	if !found {
		t.Errorf("filter %q does not include %s", r.filters[0], want)
	}
	if _, ok := r.form["extra_filters[]"]; ok {
		t.Errorf("extra_filters[] in the form body: %v", r.form)
	}
}

func TestMetricsExploreIsConfinedForDevelopersAndViewers(t *testing.T) {
	vm := &fakeVM{}
	c := metricsConsole(t, vm, "mx-explore")

	attempts := []string{
		`up{namespace="kube-system"}`,
		`{__name__=~".+"}`,
		`sum by (namespace) (container_memory_working_set_bytes{namespace=~".*"})`,
		`up or on() vector(1)`,
		`up&extra_filters[]={namespace=~".*"}`,
	}
	for _, s := range []*session{c.dev, c.viewer} {
		for _, q := range attempts {
			vm.reset()
			// The API takes nothing but query, range and step: a smuggled
			// extra_filters[] next to them is ignored.
			path := "/api/v1/metrics/query?range=1h&query=" + url.QueryEscape(q) + "&extra_filters[]=" + url.QueryEscape(`{namespace=~".*"}`)
			var out struct {
				Scope  string           `json:"scope"`
				Series []metrics.Series `json:"series"`
			}
			if code := s.do(t, "GET", path, nil, &out); code != http.StatusOK {
				t.Fatalf("query %q: %d", q, code)
			}
			if out.Scope != "projects" {
				t.Errorf("scope %q", out.Scope)
			}
			reqs := vm.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			confined(t, reqs[0], "mx-explore")
			if got := reqs[0].form.Get("query"); got != q {
				t.Errorf("VictoriaMetrics got query %q, want %q verbatim", got, q)
			}
		}
	}
}

func TestMetricsExploreIsUnconfinedForOwners(t *testing.T) {
	vm := &fakeVM{}
	c := metricsConsole(t, vm, "mx-owner")
	var out struct {
		Scope string `json:"scope"`
	}
	if code := c.owner.do(t, "GET", "/api/v1/metrics/query?query=up&range=6h", nil, &out); code != http.StatusOK || out.Scope != "all" {
		t.Fatalf("owner: %d %+v", code, out)
	}
	reqs := vm.requests()
	if len(reqs) != 1 || len(reqs[0].filters) != 0 {
		t.Fatalf("owner request %+v", reqs)
	}
	if reqs[0].form.Get("step") != "120s" {
		t.Errorf("6h step = %s", reqs[0].form.Get("step"))
	}
}

func TestAppMetrics(t *testing.T) {
	vm := &fakeVM{}
	c := metricsConsole(t, vm, "mx-app")

	var out map[string]any
	if code := c.viewer.do(t, "GET", "/api/v1/projects/mx-app/apps/web/metrics?range=24h", nil, &out); code != http.StatusOK {
		t.Fatalf("viewer: %d %v", code, out)
	}
	for _, k := range metrics.AppSeries {
		pts, ok := out[k].([]any)
		if !ok || len(pts) != 2 {
			t.Errorf("%s = %v", k, out[k])
		}
	}
	if out["step"] != float64(600) {
		t.Errorf("step %v", out["step"])
	}
	reqs := vm.requests()
	if len(reqs) != len(metrics.AppSeries) {
		t.Fatalf("%d requests, want %d", len(reqs), len(metrics.AppSeries))
	}
	for _, r := range reqs {
		// Confined to the App's namespace only, whatever else the user sees.
		if len(r.filters) != 1 || r.filters[0] != `{namespace=~"mx-app"}` {
			t.Errorf("filters %q", r.filters)
		}
		if q := r.form.Get("query"); !strings.Contains(q, `namespace="mx-app"`) {
			t.Errorf("query %s", q)
		}
	}

	// Owners are confined to the App's namespace too.
	vm.reset()
	if code := c.owner.do(t, "GET", "/api/v1/projects/mx-app/apps/web/metrics", nil, nil); code != http.StatusOK {
		t.Fatalf("owner: %d", code)
	}
	for _, r := range vm.requests() {
		if len(r.filters) != 1 || r.filters[0] != `{namespace=~"mx-app"}` {
			t.Errorf("owner filters %q", r.filters)
		}
	}

	// An App that does not exist (or another project's name in the path)
	// never reaches VictoriaMetrics.
	vm.reset()
	for _, path := range []string{"/api/v1/projects/mx-app/apps/nope/metrics", "/api/v1/projects/kube-system/apps/coredns/metrics"} {
		if code := c.dev.do(t, "GET", path, nil, nil); code != http.StatusNotFound && code != http.StatusForbidden {
			t.Errorf("%s: %d", path, code)
		}
	}
	if n := len(vm.requests()); n != 0 {
		t.Errorf("%d requests for missing apps", n)
	}
}

func TestMetricsRangeBounds(t *testing.T) {
	vm := &fakeVM{}
	c := metricsConsole(t, vm, "mx-range")
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"range=31d", http.StatusBadRequest},
		{"range=1m", http.StatusBadRequest},
		{"range=forever", http.StatusBadRequest},
		{"range=1h&step=-5s", http.StatusBadRequest},
		{"range=30d&step=1s", http.StatusOK}, // raised to keep ≤ 500 points
		{"range=7d", http.StatusOK},
	} {
		vm.reset()
		var e apiError
		if code := c.dev.do(t, "GET", "/api/v1/projects/mx-range/apps/web/metrics?"+tc.query, nil, &e); code != tc.code {
			t.Errorf("%s: %d %+v, want %d", tc.query, code, e, tc.code)
		}
		for _, r := range vm.requests() {
			var start, end, step int64
			fmt.Sscan(r.form.Get("start"), &start)
			fmt.Sscan(r.form.Get("end"), &end)
			fmt.Sscan(strings.TrimSuffix(r.form.Get("step"), "s"), &step)
			if step <= 0 || (end-start)/step+1 > metrics.MaxPoints {
				t.Errorf("%s: start %d end %d step %d", tc.query, start, end, step)
			}
		}
	}
	if code := c.owner.do(t, "GET", "/api/v1/metrics/query?range=1h", nil, nil); code != http.StatusBadRequest {
		t.Errorf("empty query: %d", code)
	}
	if code := c.owner.do(t, "GET", "/api/v1/metrics/query?query="+strings.Repeat("x", 5000), nil, nil); code != http.StatusBadRequest {
		t.Errorf("long query: %d", code)
	}
}

func TestMetricsOverview(t *testing.T) {
	vm := &fakeVM{}
	c := metricsConsole(t, vm, "mx-overview")

	type overview struct {
		Scope    string           `json:"scope"`
		Nodes    []nodeUsageJSON  `json:"nodes"`
		Platform *json0           `json:"platform"`
		TopApps  []appUsageJSON   `json:"topApps"`
		Series   map[string][]any `json:"series"`
	}
	var dev overview
	if code := c.dev.do(t, "GET", "/api/v1/metrics/overview", nil, &dev); code != http.StatusOK {
		t.Fatalf("developer: %d", code)
	}
	if dev.Scope != "projects" || len(dev.Nodes) != 0 || dev.Platform != nil {
		t.Errorf("developer sees %+v", dev)
	}
	if len(dev.TopApps) != 1 || dev.TopApps[0] != (appUsageJSON{Project: "mx-overview", App: "web", CPU: 0.25, Memory: 0.25}) {
		t.Errorf("top apps %+v", dev.TopApps)
	}
	for _, r := range vm.requests() {
		confined(t, r, "mx-overview")
		if q := r.form.Get("query"); strings.Contains(q, "node_") || strings.Contains(q, "kube-") {
			t.Errorf("developer's overview asked for %s", q)
		}
	}

	vm.reset()
	var owner overview
	if code := c.owner.do(t, "GET", "/api/v1/metrics/overview?range=6h", nil, &owner); code != http.StatusOK {
		t.Fatalf("owner: %d", code)
	}
	if owner.Scope != "all" || len(owner.Nodes) != 1 || owner.Nodes[0].Name != "node-1" || owner.Nodes[0].CPUCapacity != 4 || owner.Platform == nil {
		t.Errorf("owner sees %+v", owner)
	}
	if len(owner.Series["cpu"]) != 2 {
		t.Errorf("series %+v", owner.Series)
	}
	for _, r := range vm.requests() {
		if len(r.filters) != 0 {
			t.Errorf("owner filtered: %q", r.filters)
		}
	}
}

type json0 = map[string]any

func TestMetricsUnavailable(t *testing.T) {
	vm := &fakeVM{down: true}
	c := metricsConsole(t, vm, "mx-down")
	var e apiError
	if code := c.dev.do(t, "GET", "/api/v1/projects/mx-down/apps/web/metrics", nil, &e); code != http.StatusServiceUnavailable || !strings.Contains(e.Error, "VictoriaMetrics") {
		t.Errorf("app metrics: %d %+v", code, e)
	}
	if code := c.owner.do(t, "GET", "/api/v1/metrics/overview", nil, &e); code != http.StatusServiceUnavailable {
		t.Errorf("overview: %d %+v", code, e)
	}

	// No VictoriaMetrics configured at all.
	plain := newConsole(t)
	if code := plain.owner.do(t, "GET", "/api/v1/metrics/overview", nil, &e); code != http.StatusServiceUnavailable || e.Error != "Metrics are not set up on this console." {
		t.Errorf("unconfigured: %d %+v", code, e)
	}
}
