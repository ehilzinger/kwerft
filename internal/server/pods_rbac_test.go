// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/setup"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Pods, logs and shells against a real API server with the chart's RBAC.
// envtest has no kubelet, so pods are objects with a hand-written status, and
// logs and exec stop at "no node": what matters here is that Kubernetes,
// asked as the user, allows and refuses the right things.

// can asks the API server, as a console user, whether they may do something.
func can(t *testing.T, role, namespace, verb, group, resource, sub string) bool {
	t.Helper()
	cs, err := cluster.imp.Clientset(role+"@example.com", role)
	if err != nil {
		t.Fatal(err)
	}
	review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(context.Background(), &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace, Verb: verb, Group: group, Resource: resource, Subresource: sub,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return review.Status.Allowed
}

func waitForPodBindings(t *testing.T, project string) {
	t.Helper()
	eventually(t, func() error {
		for _, name := range []string{controllers.PodsReadRole, controllers.PodsExecRole} {
			if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: project, Name: name}, &rbacv1.RoleBinding{}); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestPodRolesFollowKubernetesRBAC(t *testing.T) {
	c := newConsole(t)
	c.project(t, "pods-rbac")
	waitForPodBindings(t, "pods-rbac")

	for _, role := range []string{"owner", "admin", "developer", "viewer"} {
		for _, check := range []struct {
			what                      string
			ns, verb, group, res, sub string
			want                      bool
		}{
			{"list pods", "pods-rbac", "list", "", "pods", "", true},
			{"read logs", "pods-rbac", "get", "", "pods", "log", true},
			{"read pod metrics", "pods-rbac", "list", "metrics.k8s.io", "pods", "", true},
			{"open a shell", "pods-rbac", "create", "", "pods", "exec", role != "viewer"},
			// The pods of the platform stay out of reach of every console role.
			{"read kube-system logs", "kube-system", "get", "", "pods", "log", false},
			{"shell in kube-system", "kube-system", "create", "", "pods", "exec", false},
			{"shell in kwerft-system", "kwerft-system", "create", "", "pods", "exec", false},
			// Exec over WebSocket is a GET: Kubernetes checks get and create.
			{"exec over WebSocket", "pods-rbac", "get", "", "pods", "exec", role != "viewer"},
			{"add a debug toolbox", "pods-rbac", "update", "", "pods", "ephemeralcontainers", role != "viewer"},
			{"debug toolbox in kube-system", "kube-system", "update", "", "pods", "ephemeralcontainers", false},
			{"attach", "pods-rbac", "create", "", "pods", "attach", false},
			{"delete pods", "pods-rbac", "delete", "", "pods", "", false},
		} {
			if got := can(t, role, check.ns, check.verb, check.group, check.res, check.sub); got != check.want {
				t.Errorf("%s may %s: %v, want %v", role, check.what, got, check.want)
			}
		}
	}

	// The console's own identity can create these bindings: it holds "bind"
	// on the project ClusterRoles, so the escalation check passes even though
	// it cannot read metrics itself.
	ctx := context.Background()
	if err := cluster.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "bind-check"}}); err != nil {
		t.Fatal(err)
	}
	console, err := client.New(cluster.console, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{controllers.PodsReadRole, controllers.PodsExecRole, controllers.ProjectDeveloperRole, controllers.ProjectViewerRole} {
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role, Namespace: "bind-check"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: "kwerft:role:viewer"}},
		}
		if err := console.Create(ctx, rb); err != nil {
			t.Errorf("console binds %s: %v", role, err)
		}
	}
}

// podConsole is newConsole with shell recordings enabled.
func newPodConsole(t *testing.T) (*console, string) {
	t.Helper()
	requireCluster(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dir := filepath.Join(t.TempDir(), "recordings")
	srv := httptest.NewServer(Handler(Config{
		UI: fstest.MapFS{"index.html": {Data: []byte("ui")}}, Logger: slog.New(slog.DiscardHandler),
		Store: st, SetupTokens: setup.NewStaticTokenSource(testToken, time.Hour), InsecureCookies: true,
		Kube: cluster.imp, KubeCache: cluster.cache, RecordingsDir: dir,
	}))
	t.Cleanup(srv.Close)
	hash, err := auth.HashPassword("a long test password")
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
		if code := s.do(t, "POST", "/api/v1/session", map[string]string{"email": email, "password": "a long test password"}, nil); code != http.StatusOK {
			t.Fatalf("sign in %s: %d", email, code)
		}
		*u.dst = s
	}
	return c, dir
}

// runningPod creates a pod object for app, as if a kubelet ran it.
func runningPod(t *testing.T, project, app, name string) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project, Labels: map[string]string{controllers.LabelApp: app, controllers.LabelProject: project}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}},
	}
	if err := cluster.admin.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "app", Ready: true, Image: "nginx:1.27", ImageID: "nginx@sha256:0",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	}}}
	if err := cluster.admin.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func dialShell(t *testing.T, s *session, path string) map[string]any {
	t.Helper()
	d := websocket.Dialer{Jar: s.client.Jar, HandshakeTimeout: 5 * time.Second}
	conn, _, err := d.Dial("ws"+strings.TrimPrefix(s.url, "http")+path, http.Header{"Origin": {s.url}})
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var started map[string]any
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var ev map[string]any
		_ = json.Unmarshal(data, &ev)
		switch ev["type"] {
		case "started":
			started = ev
		case "error":
			return ev
		case "exit":
			ev["started"] = started
			return ev
		}
	}
}

func TestReplicasLogsAndShellAsTheUser(t *testing.T) {
	c, dir := newPodConsole(t)
	c.project(t, "pods-api")
	waitForPodBindings(t, "pods-api")
	if code := c.dev.do(t, "POST", "/api/v1/projects/pods-api/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	runningPod(t, "pods-api", "web", "web-7c9d8-x2kq")
	runningPod(t, "pods-api", "other", "other-1")

	// Replicas: every role sees them; what they may do comes from RBAC.
	for _, tc := range []struct {
		s    *session
		exec bool
	}{{c.viewer, false}, {c.dev, true}, {c.owner, true}} {
		var out podsJSON
		if code := tc.s.do(t, "GET", "/api/v1/projects/pods-api/apps/web/pods", nil, &out); code != http.StatusOK {
			t.Fatalf("pods: %d", code)
		}
		if len(out.Pods) != 1 || out.Pods[0].Name != "web-7c9d8-x2kq" || out.Pods[0].Status != "Running" || !out.Access.Logs || out.Access.Exec != tc.exec {
			t.Errorf("pods = %+v", out)
		}
		// envtest has no metrics-server: the table shows dashes, not zeros.
		if out.Metrics || out.Pods[0].CPUMillis != nil {
			t.Errorf("metrics without a metrics-server: %+v", out)
		}
	}

	// Logs: allowed for a viewer; Kubernetes then has no node to ask.
	res, err := c.viewer.client.Get(c.viewer.url + "/api/v1/projects/pods-api/apps/web/logs")
	if err != nil {
		t.Fatal(err)
	}
	body := bufio.NewScanner(res.Body)
	var events []string
	for body.Scan() {
		if line := body.Text(); strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || len(events) < 2 || events[0] != "start" || events[len(events)-1] != "end" {
		t.Errorf("viewer logs: %d %v", res.StatusCode, events)
	}

	// Shell: refused for a viewer by Kubernetes RBAC, and audited.
	const shell = "/api/v1/projects/pods-api/apps/web/pods/web-7c9d8-x2kq/shell"
	if ev := dialShell(t, c.viewer, shell); ev["type"] != "error" || ev["message"] != "Your role does not allow opening a shell." {
		t.Errorf("viewer shell: %v", ev)
	}
	// A developer passes RBAC; the session is recorded and audited, and the
	// API server then answers that the pod has no node to run it on.
	ev := dialShell(t, c.dev, shell)
	started, _ := ev["started"].(map[string]any)
	if ev["type"] != "exit" || ev["reason"] != "error" || started == nil || strings.Contains(fmt.Sprint(ev["message"]), "role") {
		t.Errorf("developer shell: %v", ev)
	}
	t.Logf("developer shell without a kubelet: %v", ev["message"])
	if started != nil {
		if _, err := os.Stat(filepath.Join(dir, started["recording"].(string)+".cast")); err != nil {
			t.Errorf("no recording: %v", err)
		}
	}
	// Not a replica of this app.
	if ev := dialShell(t, c.dev, "/api/v1/projects/pods-api/apps/web/pods/other-1/shell"); ev["type"] != "error" {
		t.Errorf("shell in another app's pod: %v", ev)
	}

	entries, err := c.store.RecentAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, e := range entries {
		seen = append(seen, e.Actor+" "+e.Action+" "+e.Target)
	}
	for _, want := range []string{
		"viewer@example.com pod.exec.denied pods-api/web-7c9d8-x2kq",
		"developer@example.com pod.exec pods-api/web-7c9d8-x2kq",
		"developer@example.com pod.exec.end pods-api/web-7c9d8-x2kq",
	} {
		if !slices.Contains(seen, want) {
			t.Errorf("audit lacks %q: %v", want, seen)
		}
	}
}
