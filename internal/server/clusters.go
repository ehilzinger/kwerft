package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hubble"
	"github.com/ehilzinger/kwerft/internal/kube"
	"github.com/ehilzinger/kwerft/internal/logs"
	"github.com/ehilzinger/kwerft/internal/metrics"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Multi-cluster access (docs/phase5.md, W4). The console reaches every
// cluster the Registry (internal/clusters) knows: the management cluster
// ("local", where it runs) and remote clusters behind their agents' tunnels.
// Per cluster it holds the same things it holds for the local one — an
// Impersonator for users' requests, an informer cache for the polled lists,
// its own identity there, and clients for that cluster's metrics, logs and
// Alertmanager — rebuilt when the Registry says a cluster came or went.
//
//   - Project names are unique across clusters, so a project names its
//     cluster: every /api/v1/projects/{project}/… route is served in the
//     project's cluster (requireUser resolves it), as the user, so that
//     cluster's RBAC decides, exactly as Kubernetes decides locally.
//   - Lists visit every connected cluster (visitClusters), each confined to
//     the user's project scope there (scope.go), and carry "cluster". A
//     cluster that cannot be reached is left out and named in the
//     Kwerft-Unreachable-Clusters header (the UI asks GET
//     /api/v1/cluster-status for its banner).
//   - Routes that read one cluster's data without a project in the path
//     (metrics overview, log search, settings, firewall) take ?cluster=
//     (or ?project=, which names its cluster); without one they mean the
//     local cluster, as before.
//   - Without a Registry, or with clusters.Static, only "local" exists and
//     nothing changes: no lookups, no extra requests.

// unreachableHeader names the clusters a list could not visit.
const unreachableHeader = "Kwerft-Unreachable-Clusters"

// clusterConn is how the console reaches one cluster.
type clusterConn struct {
	name string
	kube *kube.Impersonator
	// cache serves lists (nil: none, lists go as the user).
	cache client.Reader
	// system and systemReader are Kwerft's own identity there (either may
	// be nil locally without the controller manager).
	system       client.Client
	systemReader client.Reader
	// Observability: nil means the api's local defaults (cfg.Metrics, the
	// api's VictoriaLogs client, the alerts API's Alertmanager).
	metrics      *metrics.Client
	logs         *logs.Client
	am           *alerting.Alertmanager
	alertMetrics *alerting.Metrics
	hubble       *hubble.Aggregator
	// host is the API server the conn was built for, to notice a changed
	// RESTConfig; stop ends its cache.
	host string
	stop context.CancelFunc
}

func (c *clusterConn) isLocal() bool { return c.name == clusters.Local }

// readers are where the console reads this cluster with its own identity:
// the cache first, then the uncached readers.
func (c *clusterConn) readers() []client.Reader {
	var out []client.Reader
	if c.cache != nil {
		out = append(out, c.cache)
	}
	if c.systemReader != nil {
		out = append(out, c.systemReader)
	}
	if c.system != nil {
		out = append(out, c.system)
	}
	return out
}

// get reads an object with the console's identity, from the cache when it
// has started.
func (c *clusterConn) get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	return c.read(func(r client.Reader) error { return r.Get(ctx, key, obj) })
}

// list is get for lists.
func (c *clusterConn) list(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.read(func(r client.Reader) error { return r.List(ctx, list, opts...) })
}

// liveGet reads uncached when it can: the name check of project creation.
func (c *clusterConn) liveGet(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	switch {
	case c.systemReader != nil:
		return c.systemReader.Get(ctx, key, obj)
	case c.system != nil:
		return c.system.Get(ctx, key, obj)
	}
	return c.get(ctx, key, obj)
}

func (c *clusterConn) read(do func(client.Reader) error) error {
	err := errors.New("no reader for cluster " + c.name)
	for _, r := range c.readers() {
		err = do(r)
		var notStarted *cache.ErrCacheNotStarted
		if !errors.As(err, &notStarted) {
			return err
		}
	}
	return err
}

// clusterState is a cluster as the console last saw it.
type clusterState struct {
	name string
	conn *clusterConn // nil: unreachable
	err  error        // why (logged, never shown)
}

// clusterSet keeps the connections, following the Registry.
type clusterSet struct {
	reg   clusters.Registry // nil: the local cluster only
	local *clusterConn
	log   *slog.Logger
	now   func() time.Time
	// connect builds a remote cluster's connection.
	connect func(name string, cfg *rest.Config) (*clusterConn, error)

	syncMu   sync.Mutex // one sync at a time
	indexMu  sync.Mutex // one index refresh at a time
	createMu sync.Mutex // project creation checks names across clusters

	mu        sync.RWMutex
	synced    bool
	changed   <-chan struct{}
	retryAt   time.Time
	known     []clusterState // local first
	index     map[string]string
	conflicts map[string][]string
	indexedAt time.Time
}

// retryAfter is how soon a failed List or connect is tried again.
const retryAfter = 30 * time.Second

func newClusterSet(reg clusters.Registry, local *clusterConn, log *slog.Logger, now func() time.Time) *clusterSet {
	return &clusterSet{reg: reg, local: local, log: log, now: now,
		known: []clusterState{{name: clusters.Local, conn: local}}, index: map[string]string{}, conflicts: map[string][]string{}}
}

// refresh syncs with the Registry when it changed (or a retry is due).
// Cheap otherwise: every request calls it.
func (s *clusterSet) refresh() {
	if s.reg == nil {
		return
	}
	s.mu.RLock()
	need := !s.synced || (!s.retryAt.IsZero() && s.now().After(s.retryAt))
	if !need {
		select {
		case <-s.changed:
			need = true
		default:
		}
	}
	s.mu.RUnlock()
	if need {
		s.sync()
	}
}

func (s *clusterSet) sync() {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	// The channel first: a change during List closes it, and the next
	// request syncs again.
	changed := s.reg.Changed()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := s.reg.List(ctx)
	if err != nil {
		s.log.Error("clusters: listing the registry failed", "err", err)
		s.mu.Lock()
		s.synced, s.changed, s.retryAt = true, changed, s.now().Add(retryAfter)
		s.mu.Unlock()
		return
	}
	s.mu.RLock()
	old := map[string]*clusterConn{}
	for _, st := range s.known {
		if st.conn != nil && !st.conn.isLocal() {
			old[st.name] = st.conn
		}
	}
	s.mu.RUnlock()

	next := []clusterState{{name: clusters.Local, conn: s.local}}
	kept := map[*clusterConn]bool{}
	retry := false
	for _, info := range infos {
		if info.Name == clusters.Local || slices.ContainsFunc(next, func(st clusterState) bool { return st.name == info.Name }) {
			continue
		}
		st := clusterState{name: info.Name, err: clusters.ErrUnavailable}
		if info.Connected {
			cfg, err := s.reg.RESTConfig(info.Name)
			switch {
			case err != nil:
				st.err = err
				retry = retry || !errors.Is(err, clusters.ErrUnavailable)
			case old[info.Name] != nil && old[info.Name].host == cfg.Host:
				st.conn, st.err = old[info.Name], nil
			default:
				conn, err := s.connect(info.Name, cfg)
				if err != nil {
					s.log.Error("clusters: cannot connect", "cluster", info.Name, "err", err)
					st.err, retry = err, true
				} else {
					st.conn, st.err = conn, nil
				}
			}
		}
		if st.conn != nil {
			kept[st.conn] = true
		}
		next = append(next, st)
	}
	s.mu.Lock()
	s.known, s.changed, s.synced = next, changed, true
	s.retryAt = time.Time{}
	if retry {
		s.retryAt = s.now().Add(retryAfter)
	}
	s.mu.Unlock()
	for _, conn := range old {
		if !kept[conn] && conn.stop != nil {
			conn.stop()
		}
	}
}

// states are the known clusters, local first.
func (s *clusterSet) states() []clusterState {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.known)
}

// multi: there is more than the local cluster.
func (s *clusterSet) multi() bool {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.known) > 1
}

// byName is a cluster's connection: clusters.ErrUnknown, or
// clusters.ErrUnavailable while it cannot be reached.
func (s *clusterSet) byName(name string) (*clusterConn, error) {
	if name == "" || name == clusters.Local {
		return s.local, nil
	}
	for _, st := range s.states() {
		if st.name == name {
			if st.conn == nil {
				return nil, &clusterUnreachableError{cluster: name}
			}
			return st.conn, nil
		}
	}
	return nil, &clusterUnknownError{cluster: name}
}

// clusterUnreachableError: the cluster is known but has no connection.
type clusterUnreachableError struct{ cluster, project string }

func (e *clusterUnreachableError) Error() string {
	if e.project != "" {
		return fmt.Sprintf("Project %q is in cluster %q, which cannot be reached right now. Try again when its agent is connected.", e.project, e.cluster)
	}
	return fmt.Sprintf("Cluster %q cannot be reached right now. Try again when its agent is connected.", e.cluster)
}

func (e *clusterUnreachableError) Unwrap() error { return clusters.ErrUnavailable }

type clusterUnknownError struct{ cluster string }

func (e *clusterUnknownError) Error() string {
	return fmt.Sprintf("There is no cluster %q.", e.cluster)
}

func (e *clusterUnknownError) Unwrap() error { return clusters.ErrUnknown }

// projectConflictError: a project name exists in more than one cluster
// (made outside the console). The console does not guess which one is meant.
type projectConflictError struct {
	project  string
	clusters []string
}

func (e *projectConflictError) Error() string {
	return fmt.Sprintf("A project named %q exists in the clusters %s. Project names must be unique across clusters: rename or remove one of them.",
		e.project, strings.Join(e.clusters, " and "))
}

// clusterError answers for a cluster that cannot serve a request.
func clusterError(w http.ResponseWriter, err error) {
	var unreachable *clusterUnreachableError
	var unknown *clusterUnknownError
	var conflict *projectConflictError
	switch {
	case errors.As(err, &unreachable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error(), "code": "clusterUnreachable", "cluster": unreachable.cluster})
	case errors.As(err, &unknown):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error(), "field": "cluster"})
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "projectConflict"})
	default:
		writeError(w, http.StatusServiceUnavailable, "The cluster cannot be reached right now. Try again in a moment.")
	}
}

// forProject is the cluster a project lives in. A name no cluster has is
// the local cluster's: requests then get the API server's own 404 there,
// as before multi-cluster.
func (s *clusterSet) forProject(ctx context.Context, project string) (*clusterConn, error) {
	if !s.multi() {
		return s.local, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		s.mu.RLock()
		name, ok := s.index[project]
		conflict := s.conflicts[project]
		s.mu.RUnlock()
		if len(conflict) > 0 {
			return nil, &projectConflictError{project: project, clusters: conflict}
		}
		if ok {
			conn, err := s.byName(name)
			var unknown *clusterUnknownError
			switch {
			case errors.As(err, &unknown):
				// The cluster was removed: look again.
			case err != nil:
				return nil, &clusterUnreachableError{cluster: name, project: project}
			case attempt == 1 || conn.hasProject(ctx, project):
				return conn, nil
			}
		}
		if attempt == 0 {
			s.refreshIndex(ctx)
		}
	}
	return s.local, nil
}

// hasProject: the project exists there, or the cluster could not say.
func (c *clusterConn) hasProject(ctx context.Context, project string) bool {
	err := c.get(ctx, client.ObjectKey{Name: project}, &kwerftv1.Project{})
	return !apierrors.IsNotFound(err)
}

// refreshIndex lists the Projects of every connected cluster. A project
// last seen in a cluster that cannot be listed now keeps that home, so its
// routes answer "unreachable" rather than reaching another cluster.
func (s *clusterSet) refreshIndex(ctx context.Context) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if s.now().Sub(s.indexedAt) < time.Second {
		return // someone just did
	}
	states := s.states()
	found := map[string][]string{}
	listed := map[string]bool{}
	for _, st := range states {
		if st.conn == nil {
			continue
		}
		var list kwerftv1.ProjectList
		if err := st.conn.list(ctx, &list); err != nil {
			s.log.Warn("clusters: cannot list projects", "cluster", st.name, "err", err)
			continue
		}
		listed[st.name] = true
		for _, p := range list.Items {
			found[p.Name] = append(found[p.Name], st.name)
		}
	}
	known := map[string]bool{}
	for _, st := range states {
		known[st.name] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index, conflicts := map[string]string{}, map[string][]string{}
	for p, c := range s.index {
		if known[c] && !listed[c] {
			index[p] = c
		}
	}
	for p, cs := range found {
		if prev, ok := index[p]; ok {
			cs = append(cs, prev)
		}
		if len(cs) == 1 {
			index[p] = cs[0]
			continue
		}
		slices.Sort(cs)
		conflicts[p] = cs
		delete(index, p)
	}
	s.index, s.conflicts, s.indexedAt = index, conflicts, s.now()
}

// remember records where a project was just created.
func (s *clusterSet) remember(project, cluster string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index[project] = cluster
}

// nameTaken is the cluster that already has a project of this name, "" if
// none does. Connected clusters are asked uncached; for clusters that cannot
// be reached the last known index answers.
func (s *clusterSet) nameTaken(ctx context.Context, name string) (string, error) {
	for _, st := range s.states() {
		if st.conn == nil {
			s.mu.RLock()
			home := s.index[name]
			s.mu.RUnlock()
			if home == st.name {
				return st.name, nil
			}
			continue
		}
		err := st.conn.liveGet(ctx, client.ObjectKey{Name: name}, &kwerftv1.Project{})
		switch {
		case err == nil:
			return st.name, nil
		case !apierrors.IsNotFound(err):
			return "", fmt.Errorf("cluster %s: %w", st.name, err)
		}
	}
	return "", nil
}

// ---- the request's cluster ------------------------------------------------------

type clusterKey struct{}

type clusterSelection struct {
	conn *clusterConn
	// explicit: named by the request ({project}, ?cluster=, ?project=),
	// so a list visits only this cluster.
	explicit bool
}

func withCluster(ctx context.Context, conn *clusterConn, explicit bool) context.Context {
	return context.WithValue(ctx, clusterKey{}, clusterSelection{conn: conn, explicit: explicit})
}

// conn is the cluster a request (or a list's visit) works in: the selected
// one, or the local cluster.
func (a *api) conn(ctx context.Context) *clusterConn {
	if sel, ok := ctx.Value(clusterKey{}).(clusterSelection); ok && sel.conn != nil {
		return sel.conn
	}
	return a.clusters.local
}

// selectedCluster is the cluster the request named, nil when it named none.
func selectedCluster(ctx context.Context) *clusterConn {
	if sel, ok := ctx.Value(clusterKey{}).(clusterSelection); ok && sel.explicit {
		return sel.conn
	}
	return nil
}

// inProjectCluster resolves the cluster of the route's {project}. On false
// it has answered.
func (a *api) inProjectCluster(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	project := r.PathValue("project")
	if project == "" || a.cfg.Kube == nil {
		return r, true
	}
	conn, err := a.clusters.forProject(r.Context(), project)
	if err != nil {
		clusterError(w, err)
		return r, false
	}
	return r.WithContext(withCluster(r.Context(), conn, true)), true
}

// withClusterParam serves routes without a {project} in one cluster when
// the request names one: ?cluster=<name>, or ?project=<name> (its cluster).
// A project route keeps its project's cluster whatever the query says.
func (a *api) withClusterParam(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("project") != "" || a.cfg.Kube == nil {
			next(w, r)
			return
		}
		q := r.URL.Query()
		var conn *clusterConn
		var err error
		switch {
		case q.Get("cluster") != "":
			conn, err = a.clusters.byName(q.Get("cluster"))
		case q.Get("project") != "":
			conn, err = a.clusters.forProject(r.Context(), q.Get("project"))
		default:
			next(w, r)
			return
		}
		if err != nil {
			clusterError(w, err)
			return
		}
		next(w, r.WithContext(withCluster(r.Context(), conn, true)))
	}
}

// ---- lists across clusters ------------------------------------------------------

// clusterVisit is one cluster a list visits: the user's client and project
// scope there, and a context that carries the cluster.
type clusterVisit struct {
	conn     *clusterConn
	ctx      context.Context
	c        client.Client
	scope    *projectScope
	projects []kwerftv1.Project
}

// visitClusters runs fn in every cluster a list covers: the one the request
// names, or every connected cluster. The local cluster (or a named one)
// failing fails the list as before; a remote cluster failing is left out
// like an unreachable one. Unreachable clusters are named in the
// Kwerft-Unreachable-Clusters header. On false it has answered.
func (a *api) visitClusters(w http.ResponseWriter, r *http.Request, ctx context.Context, p *principal, action, notFound string, fn func(v *clusterVisit) error) bool {
	var conns []*clusterConn
	var unreachable []string
	if sel := selectedCluster(ctx); sel != nil {
		conns = []*clusterConn{sel}
	} else {
		for _, st := range a.clusters.states() {
			if st.conn != nil {
				conns = append(conns, st.conn)
			} else {
				unreachable = append(unreachable, st.name)
			}
		}
	}
	for _, conn := range conns {
		v := &clusterVisit{conn: conn, ctx: withCluster(ctx, conn, false)}
		var err error
		if v.c, err = conn.kube.For(p.user.Email, p.user.Role); err != nil {
			a.internalError(w, r, err)
			return false
		}
		if v.scope, v.projects, err = a.scopeAndProjects(v.ctx, p); err == nil {
			err = fn(v)
		}
		if err == nil {
			continue
		}
		if conn.isLocal() || len(conns) == 1 {
			a.kubeError(w, r.WithContext(v.ctx), p, action, "", notFound, err)
			return false
		}
		a.cfg.Logger.Warn("list: cluster left out", "cluster", conn.name, "action", action, "err", err)
		unreachable = append(unreachable, conn.name)
	}
	if len(unreachable) > 0 {
		w.Header().Set(unreachableHeader, strings.Join(unreachable, ","))
	}
	return true
}

// ---- connecting a remote cluster -----------------------------------------------------

// connectCluster builds what the console needs for a remote cluster from
// the Registry's RESTConfig (Kwerft's own identity there): clients, an
// informer cache started in the background, and the observability stack
// through the cluster's service proxy.
func (a *api) connectCluster(name string, cfg *rest.Config) (*clusterConn, error) {
	scheme := controllers.NewScheme()
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, hc)
	if err != nil {
		return nil, err
	}
	imp, err := kube.NewImpersonator(cfg, hc, mapper, scheme)
	if err != nil {
		return nil, err
	}
	sys, err := client.New(cfg, client.Options{HTTPClient: hc, Mapper: mapper, Scheme: scheme})
	if err != nil {
		return nil, err
	}
	ca, err := cache.New(cfg, cache.Options{HTTPClient: hc, Mapper: mapper, Scheme: scheme})
	if err != nil {
		return nil, err
	}
	host, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil {
		return nil, err
	}
	proxy := func(svc string) (string, error) { return observability.ServiceProxyURL(host.String(), svc) }
	metricsURL, err := proxy(observability.MetricsURL)
	if err != nil {
		return nil, err
	}
	logsURL, err := proxy(observability.LogsURL)
	if err != nil {
		return nil, err
	}
	amURL, err := proxy(observability.AlertmanagerURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(a.baseCtx)
	conn := &clusterConn{
		name: name, kube: imp, cache: ca, system: sys, systemReader: sys, host: cfg.Host, stop: cancel,
		metrics:      &metrics.Client{URL: metricsURL, HTTP: hc},
		logs:         logs.NewWithHTTP(logsURL, hc),
		am:           &alerting.Alertmanager{URL: amURL, HTTP: hc},
		alertMetrics: &alerting.Metrics{URL: metricsURL, HTTP: hc},
	}
	go func() {
		if err := ca.Start(ctx); err != nil {
			a.cfg.Logger.Error("clusters: cache stopped", "cluster", name, "err", err)
		}
	}()
	if a.cfg.clusterHook != nil {
		a.cfg.clusterHook(conn)
	}
	return conn, nil
}

// ---- status for the UI --------------------------------------------------------

type clusterStatusJSON struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

// clusterStatus lists the clusters and whether each can be reached now, for
// the cluster column, filter and picker and the "unreachable" banner. Every
// signed-in user may ask: lists name clusters anyway.
func (a *api) clusterStatus(w http.ResponseWriter, _ *http.Request) {
	out := []clusterStatusJSON{}
	for _, st := range a.clusters.states() {
		out = append(out, clusterStatusJSON{Name: st.name, Connected: st.conn != nil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}
