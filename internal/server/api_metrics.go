package server

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/metrics"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Metrics API: charts and the explorer, read from VictoriaMetrics
// (internal/metrics) with the console's own identity — VictoriaMetrics has no
// users. What a user sees is decided here:
//
//   - Every query carries a namespace filter that VictoriaMetrics joins into
//     each series selector (extra_filters[]), built from the projects the user
//     may list. Owners and admins are unconfined: they also see the
//     platform's namespaces and the nodes.
//   - A per-app chart first reads the App as the user (impersonation), and
//     is confined to that App's namespace whatever the role.
//   - Only the explorer takes PromQL from the user; charts use the catalog.
//     A namespace in the user's query never widens the filter, it can only
//     narrow it further.

const (
	maxQueryLength   = 4000
	maxExploreSeries = 100
	topAppsLimit     = 50
)

func (a *api) registerMetrics(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireMetrics(h)))
	}
	mux.HandleFunc("GET /api/v1/metrics/overview", read(a.metricsOverview))
	mux.HandleFunc("GET /api/v1/metrics/query", read(a.metricsQuery))
	mux.HandleFunc("GET /api/v1/projects/{project}/apps/{app}/metrics", read(a.appMetrics))
}

func (a *api) requireMetrics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.Metrics == nil {
			writeError(w, http.StatusServiceUnavailable, "Metrics are not set up on this console.")
			return
		}
		next(w, r)
	}
}

func unconfined(p *principal) bool {
	return p.user.Role == store.RoleOwner || p.user.Role == store.RoleAdmin
}

// metricsScope is what the signed-in user's queries may see: everything for
// owners and admins, otherwise the namespaces of the projects they can list.
func (a *api) metricsScope(w http.ResponseWriter, r *http.Request) (metrics.Scope, bool) {
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return metrics.Scope{}, false
	}
	if unconfined(p) {
		return metrics.Unconfined(), true
	}
	var projects kwerftv1.ProjectList
	if err := a.list(ctx, c, &projects); err != nil {
		a.kubeError(w, r, p, "metrics.scope", "", "No projects found.", err)
		return metrics.Scope{}, false
	}
	names := make([]string, 0, len(projects.Items))
	for _, pr := range projects.Items {
		names = append(names, pr.Name) // a Project's namespace has its name
	}
	return metrics.Namespaces(names...), true
}

// metricsRange reads ?range= (default 1h) and ?step= (optional).
func metricsRange(w http.ResponseWriter, r *http.Request, now time.Time) (metrics.Range, string, bool) {
	q := r.URL.Query()
	label := cmp.Or(q.Get("range"), "1h")
	span, err := metrics.ParseDuration(label)
	if err != nil {
		writeFieldError(w, "range", "Pick a time range like 1h, 6h, 24h or 7d.")
		return metrics.Range{}, "", false
	}
	var step time.Duration
	if s := q.Get("step"); s != "" {
		if step, err = metrics.ParseDuration(s); err != nil {
			writeFieldError(w, "step", "The step must be a duration like 30s, 5m or 1h.")
			return metrics.Range{}, "", false
		}
	}
	rng, err := metrics.NewRange(now, span, step)
	if err != nil {
		writeFieldError(w, "range", strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
		return metrics.Range{}, "", false
	}
	return rng, label, true
}

// metricsError answers for a failed VictoriaMetrics request.
func (a *api) metricsError(w http.ResponseWriter, r *http.Request, err error) {
	var qe *metrics.QueryError
	switch {
	case errors.As(err, &qe):
		writeError(w, http.StatusBadRequest, qe.Message)
	case errors.Is(err, metrics.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		a.cfg.Logger.Warn("metrics unavailable", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable,
			"Metrics are unavailable right now: VictoriaMetrics (namespace kwerft-observability) did not answer. Try again in a moment.")
	default:
		a.internalError(w, r, err)
	}
}

// query is one catalog query of a chart response.
type query struct {
	key, expr string
	instant   bool
}

// runQueries runs qs in parallel; the first failure fails them all.
func (a *api) runQueries(ctx context.Context, qs []query, rng metrics.Range, scope metrics.Scope) (map[string][]metrics.Series, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
		out   = make(map[string][]metrics.Series, len(qs))
	)
	for _, q := range qs {
		wg.Go(func() {
			var s []metrics.Series
			var err error
			if q.instant {
				s, err = a.cfg.Metrics.Query(ctx, q.expr, rng.End, scope)
			} else {
				s, err = a.cfg.Metrics.QueryRange(ctx, q.expr, rng, scope)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
					cancel()
				}
				return
			}
			out[q.key] = s
		})
	}
	wg.Wait()
	return out, first
}

// points is the single series of an aggregated query, or nil (JSON null)
// when there is no data.
func points(s []metrics.Series) []metrics.Point {
	if len(s) == 0 || len(s[0].Points) == 0 {
		return nil
	}
	return s[0].Points
}

// value is the instant value of a series, 0 without data.
func value(s metrics.Series) float64 {
	if len(s.Points) == 0 {
		return 0
	}
	return s.Points[len(s.Points)-1].V
}

// ---- per app ---------------------------------------------------------------

func (a *api) appMetrics(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	rng, label, ok := metricsRange(w, r, a.now())
	if !ok {
		return
	}
	// Reading the App as the user is the permission check.
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var app kwerftv1.App
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &app); err != nil {
		a.kubeError(w, r, p, "app.metrics", appTarget(project, name), appNotFound(project, name), err)
		return
	}
	exprs, err := metrics.AppQueries(app.Namespace, app.Name, rng.Step)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	qs := make([]query, 0, len(exprs))
	for _, k := range metrics.AppSeries {
		qs = append(qs, query{key: k, expr: exprs[k]})
	}
	res, err := a.runQueries(r.Context(), qs, rng, metrics.Namespaces(app.Namespace))
	if err != nil {
		a.metricsError(w, r, err)
		return
	}
	out := map[string]any{"range": label, "step": int64(rng.Step / time.Second),
		"start": rng.Start.Unix(), "end": rng.End.Unix()}
	for _, k := range metrics.AppSeries {
		out[k] = points(res[k])
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- overview ----------------------------------------------------------------

type nodeUsageJSON struct {
	Name           string  `json:"name"`
	CPU            float64 `json:"cpu"`         // cores in use
	CPUCapacity    float64 `json:"cpuCapacity"` // cores
	Memory         float64 `json:"memory"`      // bytes in use
	MemoryCapacity float64 `json:"memoryCapacity"`
	Disk           float64 `json:"disk"` // root filesystem, bytes used
	DiskCapacity   float64 `json:"diskCapacity"`
}

type namespaceUsageJSON struct {
	Namespace string  `json:"namespace"`
	CPU       float64 `json:"cpu"`
	Memory    float64 `json:"memory"`
}

type platformUsageJSON struct {
	CPU        float64              `json:"cpu"`
	Memory     float64              `json:"memory"`
	Namespaces []namespaceUsageJSON `json:"namespaces"`
}

type appUsageJSON struct {
	Project string  `json:"project"`
	App     string  `json:"app"`
	CPU     float64 `json:"cpu"`
	Memory  float64 `json:"memory"`
}

func (a *api) metricsOverview(w http.ResponseWriter, r *http.Request) {
	rng, label, ok := metricsRange(w, r, a.now())
	if !ok {
		return
	}
	scope, ok := a.metricsScope(w, r)
	if !ok {
		return
	}
	qs := []query{
		{key: "appsCPU", expr: metrics.AppsCPU, instant: true},
		{key: "appsMemory", expr: metrics.AppsMemory, instant: true},
	}
	if scope.All() {
		qs = append(qs,
			query{key: "nodeCPU", expr: metrics.NodeCPU, instant: true},
			query{key: "nodeCPUCapacity", expr: metrics.NodeCPUCapacity, instant: true},
			query{key: "nodeMemory", expr: metrics.NodeMemory, instant: true},
			query{key: "nodeMemoryCapacity", expr: metrics.NodeMemoryCapacity, instant: true},
			query{key: "nodeDisk", expr: metrics.NodeDisk, instant: true},
			query{key: "nodeDiskCapacity", expr: metrics.NodeDiskCapacity, instant: true},
			query{key: "platformCPU", expr: metrics.PlatformCPU, instant: true},
			query{key: "platformMemory", expr: metrics.PlatformMemory, instant: true},
			query{key: "seriesCPU", expr: "sum(" + metrics.NodeCPU + ")"},
			query{key: "seriesMemory", expr: "sum(" + metrics.NodeMemory + ")"},
		)
	} else {
		qs = append(qs,
			query{key: "seriesCPU", expr: "sum(" + metrics.AppsCPU + ")"},
			query{key: "seriesMemory", expr: "sum(" + metrics.AppsMemory + ")"},
		)
	}
	res, err := a.runQueries(r.Context(), qs, rng, scope)
	if err != nil {
		a.metricsError(w, r, err)
		return
	}

	out := map[string]any{
		"range": label, "step": int64(rng.Step / time.Second), "start": rng.Start.Unix(), "end": rng.End.Unix(),
		"scope":    map[bool]string{true: "all", false: "projects"}[scope.All()],
		"nodes":    []nodeUsageJSON{},
		"platform": nil,
		"topApps":  topApps(res["appsCPU"], res["appsMemory"]),
		"series":   map[string]any{"cpu": points(res["seriesCPU"]), "memory": points(res["seriesMemory"])},
	}
	if scope.All() {
		out["nodes"] = nodeUsage(res)
		out["platform"] = platformUsage(res["platformCPU"], res["platformMemory"])
	}
	writeJSON(w, http.StatusOK, out)
}

func nodeUsage(res map[string][]metrics.Series) []nodeUsageJSON {
	byName := map[string]*nodeUsageJSON{}
	set := func(key string, f func(*nodeUsageJSON, float64)) {
		for _, s := range res[key] {
			name := s.Labels["nodename"]
			if name == "" {
				continue
			}
			n := byName[name]
			if n == nil {
				n = &nodeUsageJSON{Name: name}
				byName[name] = n
			}
			f(n, value(s))
		}
	}
	set("nodeCPU", func(n *nodeUsageJSON, v float64) { n.CPU = v })
	set("nodeCPUCapacity", func(n *nodeUsageJSON, v float64) { n.CPUCapacity = v })
	set("nodeMemory", func(n *nodeUsageJSON, v float64) { n.Memory = v })
	set("nodeMemoryCapacity", func(n *nodeUsageJSON, v float64) { n.MemoryCapacity = v })
	set("nodeDisk", func(n *nodeUsageJSON, v float64) { n.Disk = v })
	set("nodeDiskCapacity", func(n *nodeUsageJSON, v float64) { n.DiskCapacity = v })
	out := make([]nodeUsageJSON, 0, len(byName))
	for _, n := range byName {
		out = append(out, *n)
	}
	slices.SortFunc(out, func(x, y nodeUsageJSON) int { return strings.Compare(x.Name, y.Name) })
	return out
}

func platformUsage(cpu, mem []metrics.Series) platformUsageJSON {
	byNS := map[string]*namespaceUsageJSON{}
	get := func(s metrics.Series) *namespaceUsageJSON {
		ns := s.Labels["namespace"]
		if byNS[ns] == nil {
			byNS[ns] = &namespaceUsageJSON{Namespace: ns}
		}
		return byNS[ns]
	}
	out := platformUsageJSON{Namespaces: []namespaceUsageJSON{}}
	for _, s := range cpu {
		get(s).CPU = value(s)
		out.CPU += value(s)
	}
	for _, s := range mem {
		get(s).Memory = value(s)
		out.Memory += value(s)
	}
	for _, n := range byNS {
		out.Namespaces = append(out.Namespaces, *n)
	}
	slices.SortFunc(out.Namespaces, func(x, y namespaceUsageJSON) int { return cmp.Compare(y.Memory, x.Memory) })
	return out
}

// topApps merges CPU and memory per app, heaviest memory first. The
// scope already left out apps of projects the user cannot see.
func topApps(cpu, mem []metrics.Series) []appUsageJSON {
	byApp := map[[2]string]*appUsageJSON{}
	get := func(s metrics.Series) *appUsageJSON {
		k := [2]string{s.Labels["namespace"], s.Labels["app"]}
		if k[0] == "" || k[1] == "" {
			return nil
		}
		if byApp[k] == nil {
			byApp[k] = &appUsageJSON{Project: k[0], App: k[1]}
		}
		return byApp[k]
	}
	for _, s := range cpu {
		if u := get(s); u != nil {
			u.CPU = value(s)
		}
	}
	for _, s := range mem {
		if u := get(s); u != nil {
			u.Memory = value(s)
		}
	}
	out := make([]appUsageJSON, 0, len(byApp))
	for _, u := range byApp {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(x, y appUsageJSON) int {
		return cmp.Or(cmp.Compare(y.Memory, x.Memory), cmp.Compare(y.CPU, x.CPU),
			strings.Compare(x.Project, y.Project), strings.Compare(x.App, y.App))
	})
	if len(out) > topAppsLimit {
		out = out[:topAppsLimit]
	}
	return out
}

// ---- explorer ------------------------------------------------------------------

func (a *api) metricsQuery(w http.ResponseWriter, r *http.Request) {
	expr := strings.TrimSpace(r.URL.Query().Get("query"))
	if expr == "" {
		writeFieldError(w, "query", "Enter a query, like sum by (namespace) (kwerft:container_memory_working_set_bytes).")
		return
	}
	if len(expr) > maxQueryLength {
		writeFieldError(w, "query", "The query is too long.")
		return
	}
	rng, label, ok := metricsRange(w, r, a.now())
	if !ok {
		return
	}
	scope, ok := a.metricsScope(w, r)
	if !ok {
		return
	}
	series, err := a.cfg.Metrics.QueryRange(r.Context(), expr, rng, scope)
	if err != nil {
		a.metricsError(w, r, err)
		return
	}
	truncated := len(series) > maxExploreSeries
	if truncated {
		series = series[:maxExploreSeries]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query": expr, "range": label, "step": int64(rng.Step / time.Second), "start": rng.Start.Unix(), "end": rng.End.Unix(),
		"scope":  map[bool]string{true: "all", false: "projects"}[scope.All()],
		"series": series, "truncated": truncated,
	})
}
