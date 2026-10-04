package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/go-logr/logr"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/git"
	"github.com/ehilzinger/kwerft/internal/kube"
	"github.com/ehilzinger/kwerft/internal/setup"
	"github.com/ehilzinger/kwerft/internal/store"
)

// cluster is a real kube-apiserver (envtest, with RBAC) running the Project,
// App, Volume, Task and Schedule reconcilers. nil when KUBEBUILDER_ASSETS is not set.
var cluster *testCluster

type testCluster struct {
	admin   client.Client      // system:masters, for setup and assertions
	console *rest.Config       // the console's own identity, bound to the chart's kwerft-controller ClusterRole
	imp     *kube.Impersonator // built on console, as in production
	cache   client.Reader      // the manager's informer cache
}

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS not set — skipping workload API tests against a real API server (run `make test`)")
		os.Exit(m.Run())
	}
	os.Exit(runWithCluster(m))
}

func runWithCluster(m *testing.M) int {
	log.SetLogger(logr.Discard())
	fail := func(what string, err error) int {
		fmt.Println(what+":", err)
		return 1
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		return fail("locate gateway-api module", err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "charts", "kwerft", "crds"),
			filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "standard"),
			// VictoriaMetrics operator (and cert-manager stand-ins), for alerting.
			filepath.Join("..", "controllers", "testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		return fail("start envtest", err)
	}
	defer func() { _ = env.Stop() }()

	scheme := controllers.NewScheme()
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fail("admin client", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The chart's RBAC: console roles, and the console's own ClusterRole bound
	// to a test user standing in for its service account. Some roles live in
	// the console's namespace.
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllers.GatewayNamespace}}); err != nil {
		return fail("create console namespace", err)
	}
	if err := applyChartRBAC(ctx, admin); err != nil {
		return fail("apply chart RBAC", err)
	}
	user, err := env.AddUser(envtest.User{Name: "kwerft-console"}, nil)
	if err != nil {
		return fail("add console user", err)
	}
	if err := admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kwerft-controller-test"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kwerft-controller"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "kwerft-console"}},
	}); err != nil {
		return fail("bind console user", err)
	}
	imp, err := kube.NewImpersonator(user.Config(), nil, nil, scheme)
	if err != nil {
		return fail("impersonator", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		return fail("manager", err)
	}
	if err := (&controllers.ProjectReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fail("project reconciler", err)
	}
	if err := (&controllers.AppReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fail("app reconciler", err)
	}
	// Traffic rules (traffic_test.go).
	if err := (&controllers.TrafficRuleReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fail("traffic rule reconciler", err)
	}
	// Jobs (jobs_test.go). Nothing runs pods here, so Tasks stay Pending.
	if err := (&controllers.VolumeReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fail("volume reconciler", err)
	}
	if err := (&controllers.TaskReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		return fail("task reconciler", err)
	}
	if err := (&controllers.ScheduleReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fail("schedule reconciler", err)
	}
	// Git connections (git_test.go): their Secrets and RBAC live in the
	// builds namespace; the reconciler talks to each test's fake Git host.
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: builds.Namespace}}); err != nil {
		return fail("create builds namespace", err)
	}
	if err := (&controllers.GitConnectionReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		Git: &git.Factory{HTTP: &http.Client{Transport: gitHosts}}, ConsoleDomain: "console.example.com"}).SetupWithManager(mgr); err != nil {
		return fail("git connection reconciler", err)
	}
	// Alerting (alerts_test.go): rules and channels render into the
	// operator's objects in kwerft-observability, next to the stack's
	// VMAlertmanager. No default rules: tests create their own.
	if err := setupAlerting(ctx, admin); err != nil {
		return fail("alerting", err)
	}
	if err := (&controllers.AlertRuleReconciler{Client: mgr.GetClient(), ConsoleDomain: "console.example.com", NoDefaults: true}).SetupWithManager(mgr); err != nil {
		return fail("alert rule reconciler", err)
	}
	if err := (&controllers.NotificationChannelReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		return fail("notification channel reconciler", err)
	}
	// Server firewall (firewall_test.go): required rules and the agents' ConfigMaps.
	if err := (&controllers.FirewallReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), PrivateNetwork: "10.0.0.0/16"}).SetupWithManager(mgr); err != nil {
		return fail("firewall reconciler", err)
	}
	go func() { _ = mgr.Start(ctx) }()

	cluster = &testCluster{admin: admin, console: user.Config(), imp: imp, cache: mgr.GetCache()}
	return m.Run()
}

// applyChartRBAC applies the ClusterRoles and bindings from the Helm chart,
// with template lines (only labels) dropped. From rbac.yaml only the
// ClusterRole is taken; its binding names a service account.
func applyChartRBAC(ctx context.Context, c client.Client) error {
	for _, f := range []struct {
		file     string
		bindings bool
	}{{"roles.yaml", true}, {"rbac.yaml", false}} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "kwerft", "templates", f.file))
		if err != nil {
			return err
		}
		var kept []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "{{") {
				kept = append(kept, line)
			}
		}
		for _, doc := range strings.Split(strings.Join(kept, "\n"), "\n---") {
			var head metav1.TypeMeta
			if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
				return fmt.Errorf("%s: %w", f.file, err)
			}
			var obj client.Object
			switch {
			case head.Kind == "ClusterRole":
				obj = &rbacv1.ClusterRole{}
			case head.Kind == "ClusterRoleBinding" && f.bindings:
				obj = &rbacv1.ClusterRoleBinding{}
			case head.Kind == "Role" && f.bindings:
				obj = &rbacv1.Role{}
			case head.Kind == "RoleBinding" && f.bindings:
				obj = &rbacv1.RoleBinding{}
			default:
				continue
			}
			if err := yaml.UnmarshalStrict([]byte(doc), obj); err != nil {
				return fmt.Errorf("%s: %w", f.file, err)
			}
			if err := c.Create(ctx, obj); err != nil {
				return fmt.Errorf("%s: create %s %s: %w", f.file, head.Kind, obj.GetName(), err)
			}
		}
	}
	return nil
}

func requireCluster(t *testing.T) {
	t.Helper()
	if cluster == nil {
		t.Skip("envtest not available (run `make test`)")
	}
}

// ---- test console with three users ------------------------------------------

type session struct {
	url    string
	client *http.Client
}

// do sends a JSON request and decodes the response body into out (if non-nil).
func (s *session) do(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return res.StatusCode
}

type console struct {
	owner, dev, viewer *session
	store              *store.Store
}

// newConsole runs the API against the test cluster and signs in an owner, a
// developer and a viewer, each with their own cookies.
func newConsole(t *testing.T, opts ...func(*Config)) *console {
	t.Helper()
	requireCluster(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := Config{
		UI: fstest.MapFS{"index.html": {Data: []byte("ui")}}, Logger: slog.New(slog.DiscardHandler),
		Store: st, SetupTokens: setup.NewStaticTokenSource(testToken, time.Hour), InsecureCookies: true,
		Kube: cluster.imp, KubeCache: cluster.cache,
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv := httptest.NewServer(Handler(cfg))
	t.Cleanup(srv.Close)

	const pw = "a long test password"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	c := &console{store: st}
	for _, u := range []struct {
		dst  **session
		role string
	}{{&c.owner, store.RoleOwner}, {&c.dev, store.RoleDeveloper}, {&c.viewer, store.RoleViewer}} {
		email := u.role + "@example.com"
		if err := st.CreateUser(ctx, &store.User{Email: email, Name: u.role, PasswordHash: hash, Role: u.role}); err != nil {
			t.Fatal(err)
		}
		jar, _ := cookiejar.New(nil)
		s := &session{url: srv.URL, client: &http.Client{Jar: jar}}
		if code := s.do(t, "POST", "/api/v1/session", map[string]string{"email": email, "password": pw}, nil); code != http.StatusOK {
			t.Fatalf("sign in %s: %d", email, code)
		}
		*u.dst = s
	}
	return c
}

// project creates a project as the owner and waits for its namespace.
func (c *console) project(t *testing.T, name string) {
	t.Helper()
	if code := c.owner.do(t, "POST", "/api/v1/projects", map[string]string{"name": name, "displayName": "Test " + name}, nil); code != http.StatusCreated {
		t.Fatalf("create project %s: %d", name, code)
	}
	eventually(t, func() error {
		var ns corev1.Namespace
		if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: name}, &ns); err != nil {
			return err
		}
		if ns.Labels[controllers.LabelProject] != name {
			return fmt.Errorf("namespace not labelled yet")
		}
		return nil
	})
}

func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within 15s: %v", err)
}

type apiError struct {
	Error string `json:"error"`
	Field string `json:"field"`
}

func imageApp(name, image string) map[string]any {
	return map[string]any{"name": name, "spec": map[string]any{
		"source": map[string]any{"image": map[string]any{"ref": image}},
		"ports":  []map[string]any{{"container": 8080}},
	}}
}

// appAt polls the app through the API until check passes.
func appAt(t *testing.T, s *session, project, name string, check func(*kwerftv1.App) error) *kwerftv1.App {
	t.Helper()
	var app kwerftv1.App
	eventually(t, func() error {
		app = kwerftv1.App{}
		if code := s.do(t, "GET", "/api/v1/projects/"+project+"/apps/"+name, nil, &app); code != http.StatusOK {
			return fmt.Errorf("get app: %d", code)
		}
		return check(&app)
	})
	return &app
}

func atRevision(n int64, image string) func(*kwerftv1.App) error {
	return func(app *kwerftv1.App) error {
		if app.Status.Revision != n || app.Status.ObservedGeneration != app.Generation {
			return fmt.Errorf("revision %d (observed generation %d of %d), want %d", app.Status.Revision, app.Status.ObservedGeneration, app.Generation, n)
		}
		if app.Status.History[0].Image != image {
			return fmt.Errorf("revision %d runs %s, want %s", n, app.Status.History[0].Image, image)
		}
		return nil
	}
}

func deployment(t *testing.T, project, name string, check func(*appsv1.Deployment) error) {
	t.Helper()
	eventually(t, func() error {
		var d appsv1.Deployment
		if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: project, Name: name}, &d); err != nil {
			return err
		}
		return check(&d)
	})
}

// ---- tests ------------------------------------------------------------------

func TestImpersonatedIdentityReachesTheAPIServer(t *testing.T) {
	requireCluster(t)
	ctx := context.Background()
	c, err := cluster.imp.For("dev@example.com", "developer")
	if err != nil {
		t.Fatal(err)
	}
	review := &authnv1.SelfSubjectReview{}
	if err := c.Create(ctx, review); err != nil {
		t.Fatal(err)
	}
	got := review.Status.UserInfo
	if got.Username != "kwerft:dev@example.com" || !slices.Contains(got.Groups, "kwerft:role:developer") || !slices.Contains(got.Groups, "system:authenticated") {
		t.Errorf("API server sees %+v", got)
	}
	if again, _ := cluster.imp.For("dev@example.com", "developer"); again != c {
		t.Error("client for the same identity was not reused")
	}
	if _, err := cluster.imp.For("dev@example.com", "system:masters"); err == nil {
		t.Error("unknown role was accepted")
	}

	// The console's own ClusterRole may impersonate only the console's groups.
	cfg := rest.CopyConfig(cluster.console)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: "mallory", Groups: []string{"system:masters"}}
	evil, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := evil.Create(ctx, &authnv1.SelfSubjectReview{}); !apierrors.IsForbidden(err) {
		t.Errorf("impersonating system:masters: %v, want forbidden", err)
	}
}

func TestRolesAreEnforcedByKubernetesRBAC(t *testing.T) {
	c := newConsole(t)
	c.project(t, "rbac")

	var e apiError
	if code := c.viewer.do(t, "POST", "/api/v1/projects/rbac/apps", imageApp("web", "nginx:1.27"), &e); code != http.StatusForbidden || e.Error != "Your role does not allow this." {
		t.Fatalf("viewer create app: %d %+v, want 403", code, e)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/rbac/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
		t.Fatalf("developer create app: %d, want 201", code)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects", map[string]string{"name": "devproj"}, nil); code != http.StatusForbidden {
		t.Errorf("developer create project: %d, want 403", code)
	}
	if code := c.viewer.do(t, "GET", "/api/v1/projects/rbac/apps/web", nil, nil); code != http.StatusOK {
		t.Errorf("viewer get app: %d, want 200", code)
	}
	for _, req := range []struct{ method, path string }{
		{"POST", "/api/v1/projects/rbac/apps/web/restart"},
		{"PATCH", "/api/v1/projects/rbac/apps/web/scale"},
		{"DELETE", "/api/v1/projects/rbac/apps/web"},
		{"DELETE", "/api/v1/projects/rbac"},
	} {
		if code := c.viewer.do(t, req.method, req.path, map[string]int{"replicas": 2}, nil); code != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d, want 403", req.method, req.path, code)
		}
	}
	// Attempts and changes are audited with the person who made them.
	entries, err := c.store.RecentAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, en := range entries {
		seen = append(seen, en.Actor+" "+en.Action+" "+en.Target)
	}
	for _, want := range []string{"viewer@example.com app.create.denied rbac/web", "developer@example.com app.create rbac/web", "owner@example.com project.create rbac"} {
		if !slices.Contains(seen, want) {
			t.Errorf("audit lacks %q: %v", want, seen)
		}
	}

	// Deleting works for the developer, and the app is gone.
	if code := c.dev.do(t, "DELETE", "/api/v1/projects/rbac/apps/web", nil, nil); code != http.StatusNoContent {
		t.Errorf("developer delete: %d", code)
	}
	if code := c.dev.do(t, "GET", "/api/v1/projects/rbac/apps/web", nil, nil); code != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", code)
	}
}

func TestAppCreateUpdateRollback(t *testing.T) {
	c := newConsole(t)
	c.project(t, "shop")
	const path = "/api/v1/projects/shop/apps/web"

	var created kwerftv1.App
	if code := c.dev.do(t, "POST", "/api/v1/projects/shop/apps", imageApp("web", "nginx:1.27"), &created); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if created.Kind != "App" || created.Spec.Source.Image.Ref != "nginx:1.27" || created.ManagedFields != nil {
		t.Errorf("created = %+v", created)
	}
	var e apiError
	if code := c.dev.do(t, "POST", "/api/v1/projects/shop/apps", imageApp("web", "nginx:1.27"), &e); code != http.StatusConflict {
		t.Errorf("duplicate create: %d %+v, want 409", code, e)
	}
	v1 := appAt(t, c.dev, "shop", "web", atRevision(1, "nginx:1.27"))

	// Update: new image and an env var; a new revision follows.
	spec := v1.Spec.DeepCopy()
	spec.Source.Image.Ref = "nginx:1.28"
	spec.Env = []corev1.EnvVar{{Name: "GREETING", Value: "hello"}}
	var updated kwerftv1.App
	if code := c.dev.do(t, "PUT", path, map[string]any{"spec": spec, "resourceVersion": v1.ResourceVersion}, &updated); code != http.StatusOK {
		t.Fatalf("update: %d", code)
	}
	if code := c.dev.do(t, "PUT", path, map[string]any{"spec": spec, "resourceVersion": v1.ResourceVersion}, &e); code != http.StatusConflict {
		t.Errorf("update with a stale resourceVersion: %d %+v, want 409", code, e)
	}
	if code := c.dev.do(t, "PUT", path, map[string]any{"spec": spec, "generation": v1.Generation}, &e); code != http.StatusConflict {
		t.Errorf("update of a stale generation: %d %+v, want 409", code, e)
	}
	appAt(t, c.dev, "shop", "web", atRevision(2, "nginx:1.28"))

	// Roll back to revision 1: its image again, as revision 3, settings kept.
	var rolled kwerftv1.App
	if code := c.dev.do(t, "POST", path+"/rollback", map[string]int{"revision": 1}, &rolled); code != http.StatusOK {
		t.Fatalf("rollback: %d", code)
	}
	if rolled.Spec.Source.Image.Ref != "nginx:1.27" || len(rolled.Spec.Env) != 1 {
		t.Errorf("rolled back spec = %+v", rolled.Spec)
	}
	v3 := appAt(t, c.dev, "shop", "web", atRevision(3, "nginx:1.27"))
	if len(v3.Status.History) != 3 {
		t.Errorf("history = %+v", v3.Status.History)
	}
	deployment(t, "shop", "web", func(d *appsv1.Deployment) error {
		ctr := d.Spec.Template.Spec.Containers[0]
		if ctr.Image != "nginx:1.27" || len(ctr.Env) != 1 {
			return fmt.Errorf("deployment runs %s with env %v", ctr.Image, ctr.Env)
		}
		return nil
	})

	if code := c.dev.do(t, "POST", path+"/rollback", map[string]int{"revision": 3}, &e); code != http.StatusConflict {
		t.Errorf("rollback to the current image: %d %+v, want 409", code, e)
	}
	e = apiError{}
	if code := c.dev.do(t, "POST", path+"/rollback", map[string]int{"revision": 99}, &e); code != http.StatusUnprocessableEntity || e.Field != "revision" {
		t.Errorf("rollback to an unknown revision: %d %+v, want 422 on revision", code, e)
	}
}

func TestGitAppRollbackPinsTheOldBuild(t *testing.T) {
	c := newConsole(t)
	c.project(t, "builds")
	ctx := context.Background()
	const path = "/api/v1/projects/builds/apps/api"
	body := map[string]any{"name": "api", "spec": map[string]any{
		"source": map[string]any{"git": map[string]any{"repository": "https://github.com/acme/api.git"}},
	}}
	if code := c.dev.do(t, "POST", "/api/v1/projects/builds/apps", body, nil); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var list []appSummaryJSON
	eventually(t, func() error {
		c.dev.do(t, "GET", "/api/v1/apps?project=builds", nil, &list)
		if len(list) != 1 || list[0].Phase != "pending" || list[0].Reason != "AwaitingBuild" || list[0].Source.Type != "git" {
			return fmt.Errorf("summary = %+v", list)
		}
		return nil
	})

	// Stand in for the Build reconciler (Phase 2): record a build's image in
	// status, then change the spec so the App reconciler rolls it out.
	build := func(image string, replicas int, revision int64) {
		t.Helper()
		var app kwerftv1.App
		if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "builds", Name: "api"}, &app); err != nil {
			t.Fatal(err)
		}
		app.Status.Image = image
		if err := cluster.admin.Status().Update(ctx, &app); err != nil {
			t.Fatal(err)
		}
		if code := c.dev.do(t, "PATCH", path+"/scale", map[string]int{"replicas": replicas}, nil); code != http.StatusOK {
			t.Fatalf("scale: %d", code)
		}
		appAt(t, c.dev, "builds", "api", atRevision(revision, image))
	}
	build("registry.local/api:aaa", 2, 1)
	build("registry.local/api:bbb", 3, 2)

	var rolled kwerftv1.App
	if code := c.dev.do(t, "POST", path+"/rollback", map[string]int{"revision": 1}, &rolled); code != http.StatusOK {
		t.Fatalf("rollback: %d", code)
	}
	if rolled.Spec.Source.Git.PinnedImage != "registry.local/api:aaa" {
		t.Errorf("pinned image = %q", rolled.Spec.Source.Git.PinnedImage)
	}
	appAt(t, c.dev, "builds", "api", atRevision(3, "registry.local/api:aaa"))
	deployment(t, "builds", "api", func(d *appsv1.Deployment) error {
		if img := d.Spec.Template.Spec.Containers[0].Image; img != "registry.local/api:aaa" {
			return fmt.Errorf("deployment runs %s", img)
		}
		return nil
	})
}

func TestValidationErrorsNameTheField(t *testing.T) {
	c := newConsole(t)
	c.project(t, "checks")
	for _, tc := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"bad name", imageApp("Not_Valid", "nginx"), "name"},
		{"negative replicas", map[string]any{"name": "neg", "spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "nginx"}}, "replicas": -1}}, "spec.replicas"},
		{"two sources", map[string]any{"name": "both", "spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "nginx"}, "git": map[string]any{"repository": "https://x/y.git"}}}}, "spec.source"},
		{"bad env name", map[string]any{"name": "env", "spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "nginx"}}, "env": []map[string]string{{"name": "1=bad", "value": "x"}}}}, "spec.env[0].name"},
		{"bad hostname", map[string]any{"name": "host", "spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "nginx"}}, "ports": []map[string]any{{"container": 80, "public": "Not A Host"}}}}, "spec.ports[0].public"},
	} {
		var e apiError
		code := c.dev.do(t, "POST", "/api/v1/projects/checks/apps", tc.body, &e)
		t.Logf("%s: %s", tc.name, e.Error)
		if code != http.StatusUnprocessableEntity || e.Field != tc.field || e.Error == "" {
			t.Errorf("%s: %d %+v, want 422 on %s", tc.name, code, e, tc.field)
		}
	}
	var e apiError
	if code := c.dev.do(t, "POST", "/api/v1/projects/checks/apps", map[string]any{"name": "x", "spec": map[string]any{"imagee": 1}}, &e); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d %+v, want 400", code, e)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/no-such-project/apps", imageApp("web", "nginx"), &e); code != http.StatusNotFound || !strings.Contains(e.Error, "no-such-project") {
		t.Errorf("app in a missing project: %d %+v, want 404", code, e)
	}
	for _, name := range []string{"kube-system", "kwerft-system", "kwerft-builds", "kwerft-observability", "default", "traefik"} {
		if code := c.owner.do(t, "POST", "/api/v1/projects", map[string]string{"name": name}, &e); code != http.StatusUnprocessableEntity || e.Field != "name" {
			t.Errorf("reserved project name %s: %d %+v", name, code, e)
		}
	}
}

func TestListShowsAppsOfAllProjects(t *testing.T) {
	c := newConsole(t)
	c.project(t, "list-a")
	c.project(t, "list-b")
	for _, p := range []string{"list-a", "list-b"} {
		if code := c.dev.do(t, "POST", "/api/v1/projects/"+p+"/apps", imageApp("svc", "nginx:1.27"), nil); code != http.StatusCreated {
			t.Fatalf("create in %s: %d", p, code)
		}
	}
	has := func(list []appSummaryJSON, project string) bool {
		return slices.ContainsFunc(list, func(s appSummaryJSON) bool { return s.Project == project && s.Name == "svc" })
	}
	var all []appSummaryJSON
	eventually(t, func() error {
		if code := c.viewer.do(t, "GET", "/api/v1/apps", nil, &all); code != http.StatusOK {
			return fmt.Errorf("list: %d", code)
		}
		if !has(all, "list-a") || !has(all, "list-b") {
			return fmt.Errorf("list = %+v", all)
		}
		return nil
	})
	for _, s := range all {
		if s.Name == "svc" && (s.Image != "nginx:1.27" || s.Desired != 1 || s.Source.Type != "image" || s.Phase == "") {
			t.Errorf("summary = %+v", s)
		}
	}
	var one []appSummaryJSON
	c.viewer.do(t, "GET", "/api/v1/apps?project=list-a", nil, &one)
	if !has(one, "list-a") || has(one, "list-b") {
		t.Errorf("filtered list = %+v", one)
	}
	var projects []projectJSON
	eventually(t, func() error {
		c.viewer.do(t, "GET", "/api/v1/projects", nil, &projects)
		i := slices.IndexFunc(projects, func(p projectJSON) bool { return p.Name == "list-a" })
		if i < 0 || projects[i].Apps != 1 || projects[i].Phase != "ready" || projects[i].DisplayName != "Test list-a" {
			return fmt.Errorf("projects = %+v", projects)
		}
		return nil
	})
}

func TestRestartAndScaleChangeTheWorkload(t *testing.T) {
	c := newConsole(t)
	c.project(t, "ops")
	const path = "/api/v1/projects/ops/apps/worker"
	if code := c.dev.do(t, "POST", "/api/v1/projects/ops/apps", imageApp("worker", "busybox:1.37"), nil); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	appAt(t, c.dev, "ops", "worker", atRevision(1, "busybox:1.37"))
	deployment(t, "ops", "worker", func(*appsv1.Deployment) error { return nil })

	var app kwerftv1.App
	if code := c.dev.do(t, "POST", path+"/restart", nil, &app); code != http.StatusOK {
		t.Fatalf("restart: %d", code)
	}
	at := app.Annotations[kwerftv1.AnnotationRestartedAt]
	if at == "" {
		t.Fatalf("no restart annotation on %+v", app.ObjectMeta)
	}
	deployment(t, "ops", "worker", func(d *appsv1.Deployment) error {
		if got := d.Spec.Template.Annotations[kwerftv1.AnnotationRestartedAt]; got != at {
			return fmt.Errorf("pod template restarted-at = %q, want %q", got, at)
		}
		return nil
	})
	// A restart is not a new revision.
	appAt(t, c.dev, "ops", "worker", atRevision(1, "busybox:1.37"))

	if code := c.dev.do(t, "PATCH", path+"/scale", map[string]int{"replicas": 3}, &app); code != http.StatusOK || *app.Spec.Replicas != 3 {
		t.Fatalf("scale: %d %+v", code, app.Spec.Replicas)
	}
	deployment(t, "ops", "worker", func(d *appsv1.Deployment) error {
		if *d.Spec.Replicas != 3 {
			return fmt.Errorf("deployment replicas = %d", *d.Spec.Replicas)
		}
		return nil
	})
	var e apiError
	if code := c.dev.do(t, "PATCH", path+"/scale", map[string]int{"replicas": -1}, &e); code != http.StatusUnprocessableEntity || e.Field != "replicas" {
		t.Errorf("negative scale: %d %+v", code, e)
	}
	if code := c.dev.do(t, "PATCH", "/api/v1/projects/ops/apps/missing/scale", map[string]int{"replicas": 1}, &e); code != http.StatusNotFound {
		t.Errorf("scale a missing app: %d %+v", code, e)
	}
}

func TestWorkloadAPIWithoutCluster(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	if code, out := e.call(t, "GET", "/api/v1/apps", nil, nil); code != http.StatusServiceUnavailable {
		t.Errorf("apps without a cluster: %d %v, want 503", code, out)
	}
	if code, _ := e.call(t, "POST", "/api/v1/projects", map[string]string{"name": "x"}, map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("cross-site project create: %d, want 403", code)
	}
}
