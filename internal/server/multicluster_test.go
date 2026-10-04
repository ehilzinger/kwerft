package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/logs"
	"github.com/ehilzinger/kwerft/internal/store"
)

// The multi-cluster console (docs/phase5.md, W4) against two real API
// servers: the shared test cluster as "local" and a second envtest cluster,
// "edge", reached through a test Registry with the edge's console identity
// — what the agent tunnel's Registry hands the console in production. Each
// cluster runs the same reconcilers, so its RBAC follows its own Projects.

var (
	remoteOnce sync.Once
	remote     *testCluster
	remoteStop func()
	remoteErr  error
)

// requireRemote starts the second cluster on first use; TestMain stops it.
func requireRemote(t *testing.T) *testCluster {
	t.Helper()
	requireCluster(t)
	remoteOnce.Do(func() { remote, remoteStop, remoteErr = startTestCluster() })
	if remoteErr != nil {
		t.Fatal(remoteErr)
	}
	return remote
}

func stopRemoteCluster() {
	if remoteStop != nil {
		remoteStop()
	}
}

// testRegistry is a clusters.Registry whose clusters connect and disconnect
// when a test says so.
type testRegistry struct {
	mu        sync.Mutex
	names     []string
	configs   map[string]*rest.Config
	connected map[string]bool
	changed   chan struct{}
}

func newTestRegistry() *testRegistry {
	return &testRegistry{configs: map[string]*rest.Config{}, connected: map[string]bool{}, changed: make(chan struct{})}
}

func (r *testRegistry) add(name string, cfg *rest.Config) {
	r.mu.Lock()
	r.names = append(r.names, name)
	r.configs[name] = cfg
	r.mu.Unlock()
	r.setConnected(name, true)
}

func (r *testRegistry) setConnected(name string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected[name] = on
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *testRegistry) List(context.Context) ([]clusters.Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []clusters.Info{{Name: clusters.Local, Connected: true}}
	for _, n := range r.names {
		out = append(out, clusters.Info{Name: n, Connected: r.connected[n]})
	}
	return out, nil
}

func (r *testRegistry) RESTConfig(name string) (*rest.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.configs[name]
	switch {
	case !ok:
		return nil, clusters.ErrUnknown
	case !r.connected[name]:
		return nil, clusters.ErrUnavailable
	}
	return rest.CopyConfig(cfg), nil
}

func (r *testRegistry) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// multiEnv is a console managing local and edge, with fake observability
// stacks per cluster.
type multiEnv struct {
	*console
	reg        *testRegistry
	edge       *testCluster
	vm, edgeVM *fakeVM
	vl, edgeVL *fakeVictoriaLogs
	am, edgeAM *fakeAlertmanager
}

func newMultiConsole(t *testing.T, opts ...func(*Config)) *multiEnv {
	t.Helper()
	edge := requireRemote(t)
	e := &multiEnv{reg: newTestRegistry(), edge: edge, vm: &fakeVM{}, edgeVM: &fakeVM{},
		vl: newFakeVictoriaLogs(t), edgeVL: newFakeVictoriaLogs(t)}
	e.reg.add("edge", edge.console)
	localMetrics, edgeMetrics := e.vm.start(t), e.edgeVM.start(t)
	var amSrv, edgeAMSrv string
	am, srv := newFakeAlertmanager(t)
	e.am, amSrv = am, srv.URL
	am, srv = newFakeAlertmanager(t)
	e.edgeAM, edgeAMSrv = am, srv.URL
	e.console = newConsole(t, func(cfg *Config) {
		cfg.Clusters = e.reg
		cfg.BaseContext = t.Context()
		cfg.Metrics, cfg.LogsURL, cfg.RecordingsDir = localMetrics, e.vl.srv.URL, t.TempDir()
		cfg.alertsHook = func(al *alertsAPI) { al.am = &alerting.Alertmanager{URL: amSrv} }
		cfg.clusterHook = func(c *clusterConn) {
			// The service-proxy URLs of a real cluster lead nowhere in
			// envtest: point the edge's stack at its fakes.
			c.metrics, c.logs = edgeMetrics, logs.New(e.edgeVL.srv.URL)
			c.am, c.alertMetrics = &alerting.Alertmanager{URL: edgeAMSrv}, &alerting.Metrics{URL: edgeMetrics.URL}
		}
		for _, o := range opts {
			o(cfg)
		}
	})
	return e
}

// projectIn creates a project as the owner in a cluster ("" for local) and
// waits until that cluster's reconciler has set it up.
func (e *multiEnv) projectIn(t *testing.T, name, in string) {
	t.Helper()
	body := map[string]string{"name": name}
	if in != "" {
		body["cluster"] = in
	}
	var out projectJSON
	if code := e.owner.do(t, "POST", "/api/v1/projects", body, &out); code != http.StatusCreated {
		t.Fatalf("create project %s in %q: %d", name, in, code)
	}
	if want := cmpOr(in, clusters.Local); out.Cluster != want {
		t.Errorf("project %s: cluster %q, want %q", name, out.Cluster, want)
	}
	tc := cluster
	if in == "edge" {
		tc = e.edge
	}
	t.Cleanup(func() {
		_ = tc.admin.Delete(context.Background(), &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	waitForProjectIn(t, tc, name)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// waitForProjectIn is waitForProjectBindings for any test cluster.
func waitForProjectIn(t *testing.T, tc *testCluster, name string) {
	t.Helper()
	eventually(t, func() error {
		var p kwerftv1.Project
		if err := tc.admin.Get(context.Background(), client.ObjectKey{Name: name}, &p); err != nil {
			return err
		}
		c := meta.FindStatusCondition(p.Status.Conditions, controllers.ConditionReady)
		if c == nil || c.ObservedGeneration != p.Generation || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("project %s not reconciled yet: %+v", name, c)
		}
		var ns corev1.Namespace
		if err := tc.admin.Get(context.Background(), client.ObjectKey{Name: name}, &ns); err != nil {
			return err
		}
		if ns.Labels[controllers.LabelProject] != name {
			return fmt.Errorf("namespace not labelled yet")
		}
		return nil
	})
}

// withHeaders sends a request and returns the status, the headers and the body.
func (s *session) withHeaders(t *testing.T, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, out
}

type clusteredItem struct{ Name, Project, Cluster string }

func clusteredList(t *testing.T, s *session, path string) []clusteredItem {
	t.Helper()
	var out []clusteredItem
	if code := s.do(t, "GET", path, nil, &out); code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, code)
	}
	return out
}

func TestMultiClusterProjectsAndLists(t *testing.T) {
	e := newMultiConsole(t)
	edge := e.edge
	ctx := context.Background()

	var status struct {
		Clusters []clusterStatusJSON `json:"clusters"`
	}
	if code := e.viewer.do(t, "GET", "/api/v1/cluster-status", nil, &status); code != http.StatusOK ||
		!slices.Equal(status.Clusters, []clusterStatusJSON{{"local", true}, {"edge", true}}) {
		t.Fatalf("cluster status: %d %+v", code, status)
	}

	// ---- projects: a cluster each, names unique across clusters ----------------
	e.projectIn(t, "mc-home", "")
	e.projectIn(t, "mc-edge", "edge")
	if err := edge.admin.Get(ctx, client.ObjectKey{Name: "mc-edge"}, &kwerftv1.Project{}); err != nil {
		t.Errorf("mc-edge is not in the edge cluster: %v", err)
	}
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "mc-edge"}, &kwerftv1.Project{}); !apierrors.IsNotFound(err) {
		t.Errorf("mc-edge in the local cluster: %v", err)
	}
	for _, tc := range []struct {
		who  *session
		body map[string]string
		want int
		what string
	}{
		{e.owner, map[string]string{"name": "mc-edge"}, http.StatusUnprocessableEntity, "a name the edge has, locally"},
		{e.owner, map[string]string{"name": "mc-home", "cluster": "edge"}, http.StatusUnprocessableEntity, "a local name, in the edge"},
		{e.dev, map[string]string{"name": "mc-dev", "cluster": "edge"}, http.StatusForbidden, "a developer choosing a cluster"},
		{e.owner, map[string]string{"name": "mc-nowhere", "cluster": "nowhere"}, http.StatusNotFound, "an unknown cluster"},
	} {
		var out apiError
		if code := tc.who.do(t, "POST", "/api/v1/projects", tc.body, &out); code != tc.want {
			t.Errorf("%s: %d %+v, want %d", tc.what, code, out, tc.want)
		}
	}

	// ---- project routes go to the project's cluster ---------------------------
	for _, p := range []string{"mc-home", "mc-edge"} {
		if code := e.dev.do(t, "POST", "/api/v1/projects/"+p+"/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
			t.Fatalf("deploy in %s: %d", p, code)
		}
	}
	if err := edge.admin.Get(ctx, client.ObjectKey{Namespace: "mc-edge", Name: "web"}, &kwerftv1.App{}); err != nil {
		t.Errorf("the app of mc-edge is not in the edge: %v", err)
	}
	appAt(t, e.dev, "mc-edge", "web", func(*kwerftv1.App) error { return nil })
	if code := e.dev.do(t, "POST", "/api/v1/projects/mc-edge/volumes", map[string]string{"name": "data", "size": "1Gi"}, nil); code != http.StatusCreated {
		t.Errorf("volume in mc-edge: %d", code)
	}

	// ---- lists aggregate and carry the cluster -------------------------------
	eventually(t, func() error {
		got := map[string]string{}
		for _, it := range clusteredList(t, e.dev, "/api/v1/apps") {
			got[it.Project+"/"+it.Name] = it.Cluster
		}
		if got["mc-home/web"] != "local" || got["mc-edge/web"] != "edge" {
			return fmt.Errorf("apps %v", got)
		}
		return nil
	})
	var projects []projectJSON
	e.dev.do(t, "GET", "/api/v1/projects", nil, &projects)
	where := map[string]string{}
	for _, p := range projects {
		where[p.Name] = p.Cluster
	}
	if where["mc-home"] != "local" || where["mc-edge"] != "edge" {
		t.Errorf("projects: %v", where)
	}
	for path, want := range map[string]string{
		"/api/v1/apps?cluster=edge":       "edge",
		"/api/v1/apps?project=mc-home":    "local",
		"/api/v1/volumes?project=mc-edge": "edge",
	} {
		items := clusteredList(t, e.dev, path)
		if len(items) == 0 {
			t.Errorf("%s: empty", path)
		}
		for _, it := range items {
			if it.Cluster != want {
				t.Errorf("%s lists %s/%s of %s", path, it.Project, it.Name, it.Cluster)
			}
		}
	}

	// ---- the edge goes away -------------------------------------------------------
	e.reg.setConnected("edge", false)
	var gone map[string]string
	if code := e.dev.do(t, "GET", "/api/v1/projects/mc-edge/apps/web", nil, &gone); code != http.StatusServiceUnavailable ||
		gone["code"] != "clusterUnreachable" || gone["cluster"] != "edge" || !strings.Contains(gone["error"], "edge") {
		t.Errorf("an app in an unreachable cluster: %d %v", code, gone)
	}
	code, h, body := e.dev.withHeaders(t, "GET", "/api/v1/apps", nil)
	var apps []clusteredItem
	_ = json.Unmarshal(body, &apps)
	if code != http.StatusOK || h.Get(unreachableHeader) != "edge" {
		t.Errorf("apps while the edge is away: %d, %s=%q", code, unreachableHeader, h.Get(unreachableHeader))
	}
	for _, a := range apps {
		if a.Cluster != "local" {
			t.Errorf("apps while the edge is away list %+v", a)
		}
	}
	if !slices.ContainsFunc(apps, func(a clusteredItem) bool { return a.Project == "mc-home" }) {
		t.Errorf("apps while the edge is away lack the local ones: %+v", apps)
	}
	if code := e.dev.do(t, "GET", "/api/v1/apps?project=mc-edge", nil, nil); code != http.StatusServiceUnavailable {
		t.Errorf("apps of a project in the unreachable cluster: %d", code)
	}
	e.viewer.do(t, "GET", "/api/v1/cluster-status", nil, &status)
	if !slices.Equal(status.Clusters, []clusterStatusJSON{{"local", true}, {"edge", false}}) {
		t.Errorf("cluster status: %+v", status)
	}
	if code := e.owner.do(t, "POST", "/api/v1/projects", map[string]string{"name": "mc-edge"}, nil); code != http.StatusUnprocessableEntity {
		t.Errorf("taking the name of a project in the unreachable cluster: %d", code)
	}
	if code := e.owner.do(t, "POST", "/api/v1/projects", map[string]string{"name": "mc-later", "cluster": "edge"}, nil); code != http.StatusServiceUnavailable {
		t.Errorf("a project in the unreachable cluster: %d", code)
	}

	// ---- and comes back ------------------------------------------------------------
	e.reg.setConnected("edge", true)
	appAt(t, e.dev, "mc-edge", "web", func(*kwerftv1.App) error { return nil })
	if _, h, _ := e.dev.withHeaders(t, "GET", "/api/v1/apps", nil); h.Get(unreachableHeader) != "" {
		t.Errorf("still unreachable: %q", h.Get(unreachableHeader))
	}

	// A name taken in both clusters past the console (kubectl): the console
	// does not guess which project is meant.
	for _, tc := range []*testCluster{cluster, edge} {
		if err := tc.admin.Create(ctx, &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "mc-dup"}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = tc.admin.Delete(context.Background(), &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "mc-dup"}})
		})
	}
	eventually(t, func() error {
		var out map[string]string
		if code := e.owner.do(t, "GET", "/api/v1/projects/mc-dup/apps/web", nil, &out); code != http.StatusConflict || out["code"] != "projectConflict" {
			return fmt.Errorf("a project in two clusters: %d %v", code, out)
		}
		return nil
	})

	// Deleting a project deletes it in its cluster.
	if code := e.owner.do(t, "DELETE", "/api/v1/projects/mc-edge", nil, nil); code != http.StatusNoContent {
		t.Errorf("delete mc-edge: %d", code)
	}
	var p kwerftv1.Project
	if err := edge.admin.Get(ctx, client.ObjectKey{Name: "mc-edge"}, &p); err == nil && p.DeletionTimestamp == nil {
		t.Error("mc-edge was not deleted in the edge")
	}
}

// TestMultiClusterStaticRegistry: with clusters.Static the console is the
// single-cluster console it was: no cluster UI data beyond "local".
func TestMultiClusterStaticRegistry(t *testing.T) {
	c := newConsole(t, func(cfg *Config) { cfg.Clusters = &clusters.Static{Config: cluster.console} })
	var status struct {
		Clusters []clusterStatusJSON `json:"clusters"`
	}
	if code := c.viewer.do(t, "GET", "/api/v1/cluster-status", nil, &status); code != http.StatusOK ||
		!slices.Equal(status.Clusters, []clusterStatusJSON{{"local", true}}) {
		t.Fatalf("cluster status: %d %+v", code, status)
	}
	c.project(t, "mc-static")
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "mc-static"}})
	})
	var out apiError
	if code := c.owner.do(t, "POST", "/api/v1/projects", map[string]string{"name": "mc-static2", "cluster": "edge"}, &out); code != http.StatusNotFound || out.Field != "cluster" {
		t.Errorf("an unknown cluster: %d %+v", code, out)
	}
	code, h, _ := c.dev.withHeaders(t, "GET", "/api/v1/apps", nil)
	if code != http.StatusOK || h.Get(unreachableHeader) != "" {
		t.Errorf("apps: %d %q", code, h.Get(unreachableHeader))
	}
}

// ---- isolation across clusters ------------------------------------------------------

// The isolation spec of isolation_test.go, across two clusters:
//
//	mci-a  local, access Members: dana as developer
//	mci-b  edge, access Members: nobody but owners and admins
//	mci-t  edge, access Team
//
// dana (console developer) must reach mci-a and mci-t and nothing of mci-b,
// through every route — the edge's Kubernetes deciding for single objects,
// writes and streams, the console's per-cluster scope for lists, metrics,
// logs and alerts read from the edge's own stack.
const (
	mciA, mciB, mciT = "mci-a", "mci-b", "mci-t"
	mciDana          = "mci-dana@example.com"
)

func TestProjectIsolationAcrossClusters(t *testing.T) {
	e := newMultiConsole(t)
	edge := e.edge
	ctx := context.Background()
	dana := e.signIn(t, mciDana, store.RoleDeveloper)
	admin := e.signIn(t, "mci-adam@example.com", store.RoleAdmin)

	e.projectIn(t, mciA, "")
	e.projectIn(t, mciB, "edge")
	e.projectIn(t, mciT, "edge")
	for project, members := range map[string][]memberInput{mciA: {{User: mciDana, Role: "developer"}}, mciB: {}} {
		var out projectAccessJSON
		if code := e.owner.do(t, "PUT", "/api/v1/projects/"+project+"/access", map[string]any{"access": "Members", "members": members}, &out); code != http.StatusOK {
			t.Fatalf("limit %s to members: %d %+v", project, code, out)
		}
	}
	waitForProjectIn(t, cluster, mciA)
	waitForProjectIn(t, edge, mciB)
	var b kwerftv1.Project
	if err := edge.admin.Get(ctx, client.ObjectKey{Name: mciB}, &b); err != nil || b.Spec.Access != kwerftv1.ProjectAccessMembers {
		t.Fatalf("mci-b in the edge: %v %+v", err, b.Spec)
	}

	// What must not leak, in mci-b on the edge.
	app := imageApp("web", "nginx:1.27")
	app["spec"].(map[string]any)["ports"] = []map[string]any{{"container": 8080, "public": "mci-b.example.com"}}
	var task kwerftv1.Task
	for _, req := range []struct {
		path string
		body any
		out  any
	}{
		{"/apps", app, nil},
		{"/volumes", map[string]string{"name": "data", "size": "1Gi"}, nil},
		{"/tasks", imageTaskBody("busybox:1.37"), &task},
		{"/schedules", scheduleBody("backup", "15 1 * * *"), nil},
	} {
		if code := e.owner.do(t, "POST", "/api/v1/projects/"+mciB+req.path, req.body, req.out); code != http.StatusCreated {
			t.Fatalf("create %s in %s: %d", req.path, mciB, code)
		}
	}
	build := &kwerftv1.Build{
		ObjectMeta: metav1.ObjectMeta{Namespace: mciB, Name: "web-0123abc-x"},
		Spec: kwerftv1.BuildSpec{App: "web", Commit: strings.Repeat("0123abcd", 5), Trigger: "manual",
			Source: kwerftv1.BuildSource{Repository: "https://git.example.com/acme/b.git", Builder: "dockerfile"}},
	}
	if err := edge.admin.Create(ctx, build); err != nil {
		t.Fatal(err)
	}
	if err := edge.admin.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: mciB, Name: "db"}, StringData: map[string]string{"password": "b-secret"}}); err != nil {
		t.Fatal(err)
	}
	runningPodIn(t, edge, mciB, "web", "web-b-1")
	for _, p := range []string{mciA, mciT} {
		if code := dana.do(t, "POST", "/api/v1/projects/"+p+"/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
			t.Fatalf("dana deploys in %s: %d", p, code)
		}
	}
	eventually(t, func() error {
		got := map[string]bool{}
		for _, it := range clusteredList(t, e.owner, "/api/v1/apps") {
			got[it.Project] = true
		}
		if !got[mciA] || !got[mciB] || !got[mciT] {
			return fmt.Errorf("caches not filled yet: apps in %v", got)
		}
		for _, path := range isoLists {
			if len(clusteredList(t, e.owner, path+"?project="+mciB)) == 0 {
				return fmt.Errorf("cache not filled yet: nothing at %s", path)
			}
		}
		return nil
	})

	t.Run("the edge's Kubernetes refuses dana everything in mci-b", func(t *testing.T) {
		cs, err := edge.imp.Clientset(mciDana, store.RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range []authorizationv1.ResourceAttributes{
			{Namespace: mciB, Verb: "list", Resource: "pods"}, {Namespace: mciB, Verb: "get", Resource: "pods", Subresource: "log"},
			{Namespace: mciB, Verb: "create", Resource: "pods", Subresource: "exec"}, {Namespace: mciB, Verb: "get", Resource: "secrets"},
			{Namespace: mciB, Verb: "create", Group: "kwerft.dev", Resource: "apps"}, {Namespace: mciB, Verb: "list", Group: "kwerft.dev", Resource: "builds"},
			{Verb: "update", Group: "kwerft.dev", Resource: "projects"},
		} {
			r, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
				&authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &a}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if r.Status.Allowed {
				t.Errorf("dana may %s %s/%s in %q of the edge", a.Verb, a.Resource, a.Subresource, a.Namespace)
			}
		}
	})

	t.Run("lists leave out mci-b, from every cluster and with every filter", func(t *testing.T) {
		var projects []projectJSON
		dana.do(t, "GET", "/api/v1/projects", nil, &projects)
		got := names(projects, func(p projectJSON) string { return p.Name + "@" + p.Cluster })
		if !slices.Contains(got, mciA+"@local") || !slices.Contains(got, mciT+"@edge") || slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, mciB+"@") }) {
			t.Errorf("projects %v, want mci-a@local and mci-t@edge, not mci-b", got)
		}
		for _, path := range isoLists {
			for _, q := range []string{"", "?project=" + mciB, "?cluster=edge"} {
				for _, it := range clusteredList(t, dana, path+q) {
					if it.Project == mciB {
						t.Errorf("GET %s%s lists %s/%s", path, q, it.Project, it.Name)
					}
				}
			}
		}
		apps := clusteredList(t, dana, "/api/v1/apps")
		if !slices.Contains(apps, clusteredItem{"web", mciA, "local"}) || !slices.Contains(apps, clusteredItem{"web", mciT, "edge"}) {
			t.Errorf("dana's apps %+v lack her own", apps)
		}
	})

	bp := "/api/v1/projects/" + mciB
	t.Run("single objects, writes and streams in mci-b are refused by the edge", func(t *testing.T) {
		for _, req := range []struct{ method, path string }{
			{"GET", bp + "/apps/web"}, {"GET", bp + "/apps/web/pods"}, {"GET", bp + "/apps/web/logs"},
			{"GET", bp + "/apps/web/builds"}, {"GET", bp + "/apps/web/metrics"}, {"GET", bp + "/builds/" + build.Name},
			{"GET", bp + "/builds/" + build.Name + "/logs"}, {"POST", bp + "/builds/" + build.Name + "/cancel"},
			{"GET", bp + "/tasks/" + task.Name}, {"GET", bp + "/tasks/" + task.Name + "/pods"}, {"GET", bp + "/tasks/" + task.Name + "/logs"},
			{"GET", bp + "/schedules/backup"}, {"POST", bp + "/apps"}, {"POST", bp + "/tasks"}, {"POST", bp + "/apps/web/restart"},
			{"DELETE", bp + "/volumes/data"}, {"GET", bp + "/traffic"}, {"PUT", bp + "/access"}, {"POST", bp + "/members"},
			{"DELETE", bp},
		} {
			var body any
			switch {
			case strings.HasSuffix(req.path, "/apps"):
				body = imageApp("intruder", "nginx:1.27")
			case strings.HasSuffix(req.path, "/tasks"):
				body = imageTaskBody("busybox:1.37")
			case strings.HasSuffix(req.path, "/access"):
				body = map[string]any{"access": "Team"}
			case strings.HasSuffix(req.path, "/members"):
				body = map[string]string{"user": mciDana, "role": "developer"}
			}
			var out apiError
			if code := impatient(dana).do(t, req.method, req.path, body, &out); code != http.StatusForbidden {
				t.Errorf("%s %s: %d %+v, want 403", req.method, req.path, code, out)
			}
		}
		if !shellRefused(t, dana, bp+"/apps/web/pods/web-b-1/shell") {
			t.Error("a shell in mci-b was not refused")
		}
		var p kwerftv1.Project
		if err := edge.admin.Get(ctx, client.ObjectKey{Name: mciB}, &p); err != nil || p.Spec.Access != kwerftv1.ProjectAccessMembers || len(p.Spec.Members) != 0 || p.DeletionTimestamp != nil {
			t.Errorf("mci-b changed: %v %+v", err, p.Spec)
		}
	})

	t.Run("the edge's metrics and logs are confined to dana's projects there", func(t *testing.T) {
		e.vm.reset()
		e.edgeVM.reset()
		var overview struct {
			Scope, Cluster string
		}
		if code := dana.do(t, "GET", "/api/v1/metrics/overview?cluster=edge", nil, &overview); code != http.StatusOK || overview.Cluster != "edge" {
			t.Fatalf("edge overview: %d %+v", code, overview)
		}
		q := url.Values{"query": {`sum by (namespace) (kwerft:container_memory_working_set_bytes{namespace="mci-b"})`}, "cluster": {"edge"}}
		if code := dana.do(t, "GET", "/api/v1/metrics/query?"+q.Encode(), nil, nil); code != http.StatusOK {
			t.Fatalf("edge explore: %d", code)
		}
		reqs := e.edgeVM.requests()
		if len(reqs) == 0 || len(e.vm.requests()) != 0 {
			t.Fatalf("edge queries reached the edge %d times, the local VictoriaMetrics %d times", len(reqs), len(e.vm.requests()))
		}
		for _, r := range reqs {
			confined(t, r, mciT)
			if strings.Contains(r.filters[0], mciB) || strings.Contains(r.filters[0], mciA) {
				t.Errorf("edge filter %q names %s or %s", r.filters[0], mciB, mciA)
			}
		}
		e.edgeVM.reset()
		if code := dana.do(t, "GET", "/api/v1/metrics/overview", nil, nil); code != http.StatusOK {
			t.Fatalf("local overview: %d", code)
		}
		if len(e.edgeVM.requests()) != 0 {
			t.Error("the local overview asked the edge")
		}
		for _, r := range e.vm.requests() {
			confined(t, r, mciA)
		}

		e.edgeVL.set(0, "")
		if code := dana.do(t, "GET", "/api/v1/logs?cluster=edge&query="+url.QueryEscape(`namespace:mci-b OR *`), nil, nil); code != http.StatusOK {
			t.Fatalf("edge log search: %d", code)
		}
		ns := namespacesOf(e.edgeVL.confinement(t))
		if !slices.Contains(ns, mciT) || slices.Contains(ns, mciB) || slices.Contains(ns, mciA) {
			t.Errorf("edge log scope %v, want mci-t and not mci-a or mci-b", ns)
		}
		before := e.edgeVL.requests()
		for _, path := range []string{"/api/v1/logs?project=" + mciB, "/api/v1/logs/tail?project=" + mciB, "/api/v1/logs?cluster=edge&project=" + mciB} {
			if code := impatient(dana).do(t, "GET", path, nil, nil); code != http.StatusNotFound {
				t.Errorf("GET %s: %d, want 404", path, code)
			}
		}
		if after := e.edgeVL.requests(); after != before {
			t.Errorf("refused searches reached the edge's VictoriaLogs (%d requests)", after-before)
		}
	})

	t.Run("the edge's alerts, silences and rules of mci-b stay hidden", func(t *testing.T) {
		alertB := amAlert(map[string]string{"kwerft_rule": "mci-crash", "namespace": mciB, "app": "web", "severity": "critical"}, "web in mci-b crashes")
		alertT := amAlert(map[string]string{"kwerft_rule": "mci-crash", "namespace": mciT, "app": "web", "severity": "critical"}, "web in mci-t crashes")
		e.edgeAM.set(alertB, alertT)
		e.am.set()
		e.edgeAM.mu.Lock()
		e.edgeAM.silences["s-mci-b"] = alerting.Silence{ID: "s-mci-b", Matchers: []alerting.Matcher{{Name: "namespace", Value: mciB, IsEqual: boolPtr(true)}},
			StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), CreatedBy: "owner@example.com", Comment: "b"}
		e.edgeAM.mu.Unlock()
		rule := &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "mci-b-restarts"},
			Spec: kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Severity: "warning", Scope: kwerftv1.AlertScope{Apps: []string{mciB + "/web"}}}}
		if err := edge.admin.Create(ctx, rule); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, n := range []string{"mci-b-restarts", "mci-dana-t"} {
				_ = edge.admin.Delete(context.Background(), &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: n}})
			}
		})
		eventually(t, func() error {
			for _, r := range rulesOf(t, e.owner) {
				if r.Name == "mci-b-restarts" && r.Cluster == "edge" {
					return nil
				}
			}
			return fmt.Errorf("rule not in the edge's cache yet")
		})

		alerts := alertsOf(t, dana, "")
		if len(alerts) != 1 || alerts[0].Project != mciT || alerts[0].Cluster != "edge" {
			t.Errorf("dana's alerts %+v, want mci-t's in the edge only", alerts)
		}
		var silences []silenceJSON
		dana.do(t, "GET", "/api/v1/alerts/silences", nil, &silences)
		for _, s := range silences {
			if s.ID == "s-mci-b" {
				t.Error("dana sees mci-b's silence")
			}
		}
		for _, r := range rulesOf(t, dana) {
			if r.Name == "mci-b-restarts" {
				t.Error("dana sees a rule about mci-b")
			}
		}
		for _, req := range []struct {
			method, path string
			body         any
			want         int
		}{
			{"POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": alerting.Fingerprint(alertB.Labels), "duration": "1h"}, http.StatusNotFound},
			{"POST", "/api/v1/alerts/silences", map[string]any{"matchers": []map[string]any{{"name": "namespace", "value": mciB}}, "duration": "1h"}, http.StatusForbidden},
			{"POST", "/api/v1/alerts/silences", map[string]any{"matchers": []map[string]any{{"name": "namespace", "value": mciB}}, "duration": "1h", "cluster": "edge"}, http.StatusForbidden},
			{"DELETE", "/api/v1/alerts/silences/s-mci-b", nil, http.StatusNotFound},
			{"DELETE", "/api/v1/alerts/silences/s-mci-b?cluster=edge", nil, http.StatusNotFound},
			{"POST", "/api/v1/alerts/rules", map[string]any{"name": "mci-dana-b", "condition": "Restarts", "scope": map[string]any{"projects": []string{mciB}}}, http.StatusForbidden},
			{"POST", "/api/v1/alerts/rules", map[string]any{"name": "mci-dana-b", "condition": "Restarts", "cluster": "edge"}, http.StatusForbidden},
			{"PUT", "/api/v1/alerts/rules/mci-b-restarts?cluster=edge", map[string]any{"condition": "Restarts", "scope": map[string]any{"projects": []string{mciT}}}, http.StatusForbidden},
			{"DELETE", "/api/v1/alerts/rules/mci-b-restarts?cluster=edge", nil, http.StatusForbidden},
			{"POST", "/api/v1/alerts/rules", map[string]any{"name": "mci-dana-mixed", "condition": "Restarts", "scope": map[string]any{"projects": []string{mciA, mciT}}}, http.StatusUnprocessableEntity},
		} {
			if code := dana.do(t, req.method, req.path, req.body, nil); code != req.want {
				t.Errorf("%s %s %v: %d, want %d", req.method, req.path, req.body, code, req.want)
			}
		}
		e.edgeAM.mu.Lock()
		_, still := e.edgeAM.silences["s-mci-b"]
		e.edgeAM.mu.Unlock()
		if !still {
			t.Error("mci-b's silence was deleted")
		}
		// dana works in mci-t: her silence and her rule go to the edge.
		var s silenceJSON
		if code := dana.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": alerting.Fingerprint(alertT.Labels), "duration": "1h"}, &s); code != http.StatusCreated || s.Cluster != "edge" {
			t.Errorf("dana silences an mci-t alert: %d %+v", code, s)
		}
		var r ruleJSON
		if code := dana.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"name": "mci-dana-t", "condition": "Restarts", "scope": map[string]any{"projects": []string{mciT}}}, &r); code != http.StatusCreated || r.Cluster != "edge" {
			t.Errorf("dana writes a rule for mci-t: %d %+v", code, r)
		}
		if err := edge.admin.Get(ctx, client.ObjectKey{Name: "mci-dana-t"}, &kwerftv1.AlertRule{}); err != nil {
			t.Errorf("dana's rule is not in the edge: %v", err)
		}
		// Owners and admins see both clusters' alerts.
		for _, u := range []*session{e.owner, admin} {
			got := alertsOf(t, u, "")
			if !slices.ContainsFunc(got, func(a alertJSON) bool { return a.Project == mciB && a.Cluster == "edge" }) {
				t.Errorf("a platform user does not see mci-b's alert: %+v", got)
			}
		}
	})

	t.Run("kubectl through the console: the edge decides, tokens stay in their projects' cluster", func(t *testing.T) {
		token := func(projects []string) string {
			var created struct {
				Token string `json:"token"`
			}
			body := map[string]any{"name": "mci " + strings.Join(projects, ","), "role": "developer"}
			if projects != nil {
				body["projects"] = projects
			}
			if code := dana.do(t, "POST", "/api/v1/account/tokens", body, &created); code != http.StatusCreated {
				t.Fatalf("token: %d", code)
			}
			return created.Token
		}
		get := func(tok, path string) int {
			req, _ := http.NewRequest("GET", dana.url+path, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			return res.StatusCode
		}
		all, onlyA, onlyT := token(nil), token([]string{mciA}), token([]string{mciT})
		for _, tc := range []struct {
			tok, path string
			want      int
		}{
			{all, "/k8s/clusters/edge/api/v1/namespaces/" + mciB + "/pods", http.StatusForbidden},
			{all, "/k8s/clusters/edge/apis/kwerft.dev/v1alpha1/namespaces/" + mciB + "/apps", http.StatusForbidden},
			{all, "/k8s/clusters/edge/apis/kwerft.dev/v1alpha1/namespaces/" + mciT + "/apps", http.StatusOK},
			{all, "/k8s/clusters/nowhere/api/v1/namespaces/" + mciT + "/pods", http.StatusNotFound},
			{onlyA, "/k8s/apis/kwerft.dev/v1alpha1/namespaces/" + mciA + "/apps", http.StatusOK},
			{onlyA, "/k8s/clusters/edge/apis/kwerft.dev/v1alpha1/namespaces/" + mciA + "/apps", http.StatusForbidden},
			{onlyT, "/k8s/apis/kwerft.dev/v1alpha1/namespaces/" + mciT + "/apps", http.StatusForbidden},
			{onlyT, "/k8s/clusters/edge/apis/kwerft.dev/v1alpha1/namespaces/" + mciT + "/apps", http.StatusOK},
		} {
			if code := get(tc.tok, tc.path); code != tc.want {
				t.Errorf("GET %s: %d, want %d", tc.path, code, tc.want)
			}
		}
		// A token limited to mci-t lists only mci-t, in the edge.
		req, _ := http.NewRequest("GET", dana.url+"/api/v1/apps", nil)
		req.Header.Set("Authorization", "Bearer "+onlyT)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var apps []clusteredItem
		_ = json.NewDecoder(res.Body).Decode(&apps)
		if len(apps) == 0 {
			t.Error("the mci-t token lists no apps")
		}
		for _, a := range apps {
			if a.Project != mciT || a.Cluster != "edge" {
				t.Errorf("the mci-t token lists %+v", a)
			}
		}
	})

	t.Run("owners and admins reach every project in both clusters", func(t *testing.T) {
		for _, u := range []*session{e.owner, admin} {
			var projects []projectJSON
			u.do(t, "GET", "/api/v1/projects", nil, &projects)
			got := names(projects, func(p projectJSON) string { return p.Name + "@" + p.Cluster })
			for _, want := range []string{mciA + "@local", mciB + "@edge", mciT + "@edge"} {
				if !slices.Contains(got, want) {
					t.Errorf("projects %v lack %s", got, want)
				}
			}
			if code := u.do(t, "GET", bp+"/apps/web", nil, nil); code != http.StatusOK {
				t.Errorf("GET an mci-b app: %d", code)
			}
			var access projectAccessJSON
			if code := u.do(t, "GET", bp+"/access", nil, &access); code != http.StatusOK || access.Access != "Members" {
				t.Errorf("mci-b access: %d %+v", code, access)
			}
		}
	})
}

func rulesOf(t *testing.T, s *session) []ruleJSON {
	t.Helper()
	var list []ruleJSON
	if code := s.do(t, "GET", "/api/v1/alerts/rules", nil, &list); code != http.StatusOK {
		t.Fatalf("list rules: %d", code)
	}
	return list
}

// runningPodIn is runningPod in any test cluster.
func runningPodIn(t *testing.T, tc *testCluster, project, app, name string) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project, Labels: map[string]string{controllers.LabelApp: app, controllers.LabelProject: project}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}},
	}
	if err := tc.admin.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "app", Ready: true, Image: "nginx:1.27", ImageID: "nginx@sha256:0",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	}}}
	if err := tc.admin.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}
