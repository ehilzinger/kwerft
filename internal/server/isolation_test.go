package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Project isolation: the security spec behind Phase 4's first exit criterion
// (docs/phase4.md): "A developer in one project cannot read another project's
// pods, logs, secrets, metrics, alerts, builds or traffic — proven by an
// automated test suite against a real API server."
//
// The cast:
//
//	iso-a     access Members: dana (console developer) as developer,
//	          vera (console viewer) as viewer
//	iso-b     access Members: nobody but owners and admins
//	iso-team  access Team: every console member with their console role
//
// dana and vera must reach iso-a and iso-team and nothing of iso-b — asked of
// Kubernetes directly (SelfSubjectAccessReview and real requests, as the
// console impersonates them) and of every console endpoint that lists,
// searches or streams, with VictoriaMetrics, VictoriaLogs and Alertmanager
// faked so the filters the console sends can be checked. Owners and admins
// reach all three.

const (
	isoA, isoB, isoTeam = "iso-a", "iso-b", "iso-team"
	dana, vera, adam    = "iso-dana@example.com", "iso-vera@example.com", "iso-adam@example.com"
)

// isoLists are the polled lists (overview, Apps, Jobs, Volumes pages).
var isoLists = []string{"/api/v1/apps", "/api/v1/tasks", "/api/v1/volumes", "/api/v1/schedules", "/api/v1/domains"}

// listed is what every list item has.
type listed struct{ Name, Project string }

type isolationEnv struct {
	*console
	dana, vera, admin *session
	vm                *fakeVM
	vl                *fakeVictoriaLogs
	am                *fakeAlertmanager
	build, task       string // names of a Build and a Task in iso-b
}

// Secret values the suite plants: no endpoint but reveal may ever answer
// with them.
const (
	isoValueA = "iso-a-value-0f9e8d"
	isoValueB = "iso-b-value-7c6b5a"
)

// signIn adds a console account and signs it in.
func (c *console) signIn(t *testing.T, email, role string) *session {
	t.Helper()
	const pw = "a long test password"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.store.CreateUser(context.Background(), &store.User{Email: email, Name: strings.Split(email, "@")[0], PasswordHash: hash, Role: role}); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	s := &session{url: c.owner.url, client: &http.Client{Jar: jar}}
	if code := s.do(t, "POST", "/api/v1/session", map[string]string{"email": email, "password": pw}, nil); code != http.StatusOK {
		t.Fatalf("sign in %s: %d", email, code)
	}
	return s
}

func newIsolationEnv(t *testing.T) *isolationEnv {
	t.Helper()
	requireCluster(t)
	e := &isolationEnv{vm: &fakeVM{ns: isoA}, vl: newFakeVictoriaLogs(t)}
	am, amSrv := newFakeAlertmanager(t)
	e.am = am
	metricsClient := e.vm.start(t)
	e.console = newConsole(t, func(cfg *Config) {
		cfg.Metrics = metricsClient
		cfg.LogsURL = e.vl.srv.URL
		cfg.RecordingsDir = t.TempDir()
		cfg.alertsHook = func(al *alertsAPI) { al.am = &alerting.Alertmanager{URL: amSrv.URL} }
	})
	e.dana = e.signIn(t, dana, store.RoleDeveloper)
	e.vera = e.signIn(t, vera, store.RoleViewer)
	e.admin = e.signIn(t, adam, store.RoleAdmin)

	ctx := context.Background()
	for _, p := range []string{isoA, isoB, isoTeam} {
		e.project(t, p)
		t.Cleanup(func() { _ = cluster.admin.Delete(ctx, &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: p}}) })
	}
	// Owners limit iso-a and iso-b to their members, through the console.
	for project, members := range map[string][]memberInput{
		isoA: {{User: dana, Role: "developer"}, {User: vera, Role: "viewer"}},
		isoB: {},
	} {
		var out projectAccessJSON
		if code := e.owner.do(t, "PUT", "/api/v1/projects/"+project+"/access", map[string]any{"access": "Members", "members": members}, &out); code != http.StatusOK {
			t.Fatalf("limit %s to members: %d %+v", project, code, out)
		}
		waitForProjectBindings(t, project)
	}

	// Something of every kind in iso-b, made by the owner: what must not leak.
	app := imageApp("web", "nginx:1.27")
	app["spec"].(map[string]any)["ports"] = []map[string]any{{"container": 8080, "public": "iso-b.example.com"}}
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
		if code := e.owner.do(t, "POST", "/api/v1/projects/"+isoB+req.path, req.body, req.out); code != http.StatusCreated {
			t.Fatalf("create %s in %s: %d", req.path, isoB, code)
		}
	}
	e.task = task.Name
	build := &kwerftv1.Build{
		ObjectMeta: metav1.ObjectMeta{Namespace: isoB, Name: "web-0123abc-x"},
		Spec: kwerftv1.BuildSpec{App: "web", Commit: strings.Repeat("0123abcd", 5), Trigger: "manual",
			Source: kwerftv1.BuildSource{Repository: "https://git.example.com/acme/b.git", Builder: "dockerfile"}},
	}
	if err := cluster.admin.Create(ctx, build); err != nil {
		t.Fatal(err)
	}
	e.build = build.Name
	if err := cluster.admin.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: isoB, Name: "db"}, StringData: map[string]string{"password": "b-secret"}}); err != nil {
		t.Fatal(err)
	}
	runningPod(t, isoB, "web", "web-b-1")
	// A secret set with a value in iso-b (the owner's) and in iso-a (dana's).
	for _, s := range []struct {
		who            *session
		project, value string
	}{{e.owner, isoB, isoValueB}, {e.dana, isoA, isoValueA}} {
		if code := s.who.do(t, "POST", "/api/v1/projects/"+s.project+"/secret-sets", map[string]any{"name": "vault"}, nil); code != http.StatusCreated {
			t.Fatalf("create a secret set in %s: %d", s.project, code)
		}
		if code := s.who.do(t, "PUT", "/api/v1/projects/"+s.project+"/secret-sets/vault/keys/API_KEY", map[string]any{"value": s.value}, nil); code != http.StatusOK {
			t.Fatalf("set a value in %s: %d", s.project, code)
		}
	}

	// And an app in each project dana reaches, made by dana herself.
	for _, p := range []string{isoA, isoTeam} {
		if code := e.dana.do(t, "POST", "/api/v1/projects/"+p+"/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
			t.Fatalf("dana deploys in %s: %d", p, code)
		}
	}
	// The owner's lists come from the informer cache: wait until it has
	// everything, so that an empty answer below means "filtered".
	eventually(t, func() error {
		var apps []appSummaryJSON
		e.owner.do(t, "GET", "/api/v1/apps", nil, &apps)
		got := map[string]bool{}
		for _, a := range apps {
			got[a.Project] = true
		}
		if !got[isoA] || !got[isoB] || !got[isoTeam] {
			return fmt.Errorf("cache not filled yet: apps in %v", got)
		}
		for _, path := range isoLists {
			var items []listed
			if e.owner.do(t, "GET", path+"?project="+isoB, nil, &items); len(items) == 0 {
				return fmt.Errorf("cache not filled yet: nothing at %s", path)
			}
		}
		return nil
	})
	return e
}

// users are the people the spec is about: who, as whom the console reaches
// Kubernetes, and their session.
type isoUser struct {
	name, email, role string
	s                 *session
}

func (e *isolationEnv) members() []isoUser {
	return []isoUser{{"dana (developer of iso-a)", dana, store.RoleDeveloper, e.dana}, {"vera (viewer of iso-a)", vera, store.RoleViewer, e.vera}}
}

// impatient is the session with a deadline: should a stream (a log tail)
// ever be let through, the spec fails instead of waiting for it to end.
func impatient(s *session) *session {
	return &session{url: s.url, client: &http.Client{Jar: s.client.Jar, Timeout: 10 * time.Second}}
}

func (e *isolationEnv) platform() []isoUser {
	return []isoUser{{"owner", "owner@example.com", store.RoleOwner, e.owner}, {"admin", adam, store.RoleAdmin, e.admin}}
}

// review asks the API server whether the user may do something.
func review(t *testing.T, u isoUser, attrs authorizationv1.ResourceAttributes) bool {
	t.Helper()
	cs, err := cluster.imp.Clientset(u.email, u.role)
	if err != nil {
		t.Fatal(err)
	}
	r, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(context.Background(),
		&authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r.Status.Allowed
}

func TestProjectIsolation(t *testing.T) {
	e := newIsolationEnv(t)

	// ---- Kubernetes: what the impersonated identity may do -----------------------

	type kcheck struct {
		what                      string
		verb, group, res, sub, ns string
	}
	// Everything namespaced that a project holds.
	inB := func() []kcheck {
		var out []kcheck
		for _, verb := range []string{"get", "list", "watch"} {
			for _, r := range []struct{ group, res, sub string }{
				{"", "pods", ""}, {"", "pods", "log"}, {"", "secrets", ""}, {"", "configmaps", ""},
				{"", "events", ""}, {"events.k8s.io", "events", ""}, {"metrics.k8s.io", "pods", ""},
				{"kwerft.dev", "apps", ""}, {"kwerft.dev", "tasks", ""}, {"kwerft.dev", "schedules", ""},
				{"kwerft.dev", "builds", ""}, {"kwerft.dev", "domains", ""}, {"kwerft.dev", "volumes", ""},
				{"kwerft.dev", "trafficrules", ""}, {"kwerft.dev", "secretsets", ""},
			} {
				out = append(out, kcheck{verb + " " + strings.TrimSuffix(r.res+"/"+r.sub, "/"), verb, r.group, r.res, r.sub, isoB})
			}
		}
		return append(out,
			kcheck{"exec (SPDY)", "create", "", "pods", "exec", isoB},
			kcheck{"exec (WebSocket)", "get", "", "pods", "exec", isoB},
			kcheck{"attach a debug container", "update", "", "pods", "ephemeralcontainers", isoB},
			kcheck{"deploy an app", "create", "kwerft.dev", "apps", "", isoB},
			kcheck{"start a build", "create", "kwerft.dev", "builds", "", isoB},
			kcheck{"run a task", "create", "kwerft.dev", "tasks", "", isoB},
			kcheck{"write a traffic rule", "create", "kwerft.dev", "trafficrules", "", isoB},
			kcheck{"create a secret set", "create", "kwerft.dev", "secretsets", "", isoB},
			kcheck{"set a secret value", "patch", "", "secrets", "", isoB},
			kcheck{"add themselves to a project", "update", "kwerft.dev", "projects", "", ""},
			kcheck{"patch a project's members", "patch", "kwerft.dev", "projects", "", ""},
		)
	}
	t.Run("Kubernetes refuses members everything in another project", func(t *testing.T) {
		for _, u := range e.members() {
			for _, c := range inB() {
				if review(t, u, authorizationv1.ResourceAttributes{Namespace: c.ns, Verb: c.verb, Group: c.group, Resource: c.res, Subresource: c.sub}) {
					t.Errorf("%s may %s in %q", u.name, c.what, c.ns)
				}
			}
		}
	})
	t.Run("Kubernetes gives members their project role in their projects", func(t *testing.T) {
		for _, tc := range []struct {
			u    isoUser
			ns   string
			c    kcheck
			want bool
		}{
			{e.members()[0], isoA, kcheck{what: "list pods", verb: "list", res: "pods"}, true},
			{e.members()[0], isoA, kcheck{what: "read logs", verb: "get", res: "pods", sub: "log"}, true},
			{e.members()[0], isoA, kcheck{what: "open a shell", verb: "create", res: "pods", sub: "exec"}, true},
			{e.members()[0], isoA, kcheck{what: "deploy", verb: "create", group: "kwerft.dev", res: "apps"}, true},
			{e.members()[0], isoA, kcheck{what: "read Secrets", verb: "get", res: "secrets"}, false},
			{e.members()[1], isoA, kcheck{what: "list apps", verb: "list", group: "kwerft.dev", res: "apps"}, true},
			{e.members()[1], isoA, kcheck{what: "read logs", verb: "get", res: "pods", sub: "log"}, true},
			{e.members()[1], isoA, kcheck{what: "open a shell", verb: "create", res: "pods", sub: "exec"}, false},
			{e.members()[1], isoA, kcheck{what: "deploy", verb: "create", group: "kwerft.dev", res: "apps"}, false},
			// A Team project: everyone, with their console role.
			{e.members()[0], isoTeam, kcheck{what: "deploy", verb: "create", group: "kwerft.dev", res: "apps"}, true},
			{e.members()[1], isoTeam, kcheck{what: "list pods", verb: "list", res: "pods"}, true},
			{e.members()[1], isoTeam, kcheck{what: "deploy", verb: "create", group: "kwerft.dev", res: "apps"}, false},
		} {
			c := tc.c
			if got := review(t, tc.u, authorizationv1.ResourceAttributes{Namespace: tc.ns, Verb: c.verb, Group: c.group, Resource: c.res, Subresource: c.sub}); got != tc.want {
				t.Errorf("%s may %s in %s: %v, want %v", tc.u.name, c.what, tc.ns, got, tc.want)
			}
		}
	})
	t.Run("Kubernetes gives owners and admins every project, but no Secrets", func(t *testing.T) {
		for _, u := range e.platform() {
			for _, ns := range []string{isoA, isoB, isoTeam} {
				for _, c := range []kcheck{
					{what: "list pods", verb: "list", res: "pods"}, {what: "read logs", verb: "get", res: "pods", sub: "log"},
					{what: "open a shell", verb: "create", res: "pods", sub: "exec"}, {what: "deploy", verb: "create", group: "kwerft.dev", res: "apps"},
				} {
					if !review(t, u, authorizationv1.ResourceAttributes{Namespace: ns, Verb: c.verb, Group: c.group, Resource: c.res, Subresource: c.sub}) {
						t.Errorf("%s may not %s in %s", u.name, c.what, ns)
					}
				}
				if review(t, u, authorizationv1.ResourceAttributes{Namespace: ns, Verb: "get", Resource: "secrets"}) {
					t.Errorf("%s may read Secrets in %s", u.name, ns)
				}
				// Only the secret sets' Secrets, by name: reveal, after the
				// password, through the console.
				if ns != isoTeam && !review(t, u, authorizationv1.ResourceAttributes{Namespace: ns, Verb: "get", Resource: "secrets", Name: "vault"}) {
					t.Errorf("%s cannot reveal a secret set's value in %s", u.name, ns)
				}
			}
		}
	})
	t.Run("members write secret values in their projects but never read them", func(t *testing.T) {
		dev, viewer := e.members()[0], e.members()[1]
		for _, tc := range []struct {
			u    isoUser
			verb string
			want bool
		}{{dev, "patch", true}, {dev, "get", false}, {viewer, "patch", false}, {viewer, "get", false}} {
			got := review(t, tc.u, authorizationv1.ResourceAttributes{Namespace: isoA, Verb: tc.verb, Resource: "secrets", Name: "vault"})
			if got != tc.want {
				t.Errorf("%s may %s the secret set's Secret in iso-a: %v, want %v", tc.u.name, tc.verb, got, tc.want)
			}
		}
		for _, verb := range []string{"list", "watch"} {
			if review(t, dev, authorizationv1.ResourceAttributes{Namespace: isoA, Verb: verb, Resource: "secrets"}) {
				t.Errorf("%s may %s Secrets in iso-a", dev.name, verb)
			}
		}
	})
	t.Run("real requests as a member of iso-a are forbidden in iso-b", func(t *testing.T) {
		ctx := context.Background()
		for _, u := range e.members() {
			c, err := cluster.imp.For(u.email, u.role)
			if err != nil {
				t.Fatal(err)
			}
			cs, err := cluster.imp.Clientset(u.email, u.role)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := cluster.imp.RESTConfig(u.email, u.role)
			if err != nil {
				t.Fatal(err)
			}
			wc, err := client.NewWithWatch(cfg, client.Options{Scheme: cluster.admin.Scheme()})
			if err != nil {
				t.Fatal(err)
			}
			for _, req := range []struct {
				what string
				do   func() error
			}{
				{"list pods", func() error { _, err := cs.CoreV1().Pods(isoB).List(ctx, metav1.ListOptions{}); return err }},
				{"read a pod's log", func() error {
					return cs.CoreV1().Pods(isoB).GetLogs("web-b-1", &corev1.PodLogOptions{}).Do(ctx).Error()
				}},
				{"read a Secret", func() error { _, err := cs.CoreV1().Secrets(isoB).Get(ctx, "db", metav1.GetOptions{}); return err }},
				{"read a secret set's Secret", func() error { _, err := cs.CoreV1().Secrets(isoB).Get(ctx, "vault", metav1.GetOptions{}); return err }},
				{"list secret sets", func() error { return c.List(ctx, &kwerftv1.SecretSetList{}, client.InNamespace(isoB)) }},
				{"list events", func() error { _, err := cs.CoreV1().Events(isoB).List(ctx, metav1.ListOptions{}); return err }},
				{"list apps", func() error { return c.List(ctx, &kwerftv1.AppList{}, client.InNamespace(isoB)) }},
				{"list apps everywhere", func() error { return c.List(ctx, &kwerftv1.AppList{}) }},
				{"watch apps", func() error {
					w, err := wc.Watch(ctx, &kwerftv1.AppList{}, client.InNamespace(isoB))
					if err == nil {
						w.Stop()
					}
					return err
				}},
				{"get a build", func() error { return c.Get(ctx, client.ObjectKey{Namespace: isoB, Name: e.build}, &kwerftv1.Build{}) }},
				{"get a task", func() error { return c.Get(ctx, client.ObjectKey{Namespace: isoB, Name: e.task}, &kwerftv1.Task{}) }},
				{"list domains", func() error { return c.List(ctx, &kwerftv1.DomainList{}, client.InNamespace(isoB)) }},
				{"list volumes", func() error { return c.List(ctx, &kwerftv1.VolumeList{}, client.InNamespace(isoB)) }},
				{"add themselves to iso-b", func() error {
					var p kwerftv1.Project
					if err := c.Get(ctx, client.ObjectKey{Name: isoB}, &p); err != nil {
						return err
					}
					p.Spec.Members = append(p.Spec.Members, kwerftv1.ProjectMember{User: u.email, Role: "developer"})
					return c.Update(ctx, &p)
				}},
			} {
				if err := req.do(); !apierrors.IsForbidden(err) {
					t.Errorf("%s: %s in %s: %v, want Forbidden", u.name, req.what, isoB, err)
				}
			}
		}
	})

	// ---- the console API ---------------------------------------------------------

	// Lists, as the overview, Apps, Jobs and Volumes pages poll them. They
	// come from the informer cache, which holds every project.
	lists := isoLists
	t.Run("console lists leave out the projects a user does not reach", func(t *testing.T) {
		for _, u := range e.members() {
			var projects []projectJSON
			if code := u.s.do(t, "GET", "/api/v1/projects", nil, &projects); code != http.StatusOK {
				t.Fatalf("%s: projects: %d", u.name, code)
			}
			got := names(projects, func(p projectJSON) string { return p.Name })
			if !slices.Contains(got, isoA) || !slices.Contains(got, isoTeam) || slices.Contains(got, isoB) {
				t.Errorf("%s: projects %v, want iso-a and iso-team, not iso-b", u.name, got)
			}
			for _, p := range projects {
				if p.Members != nil {
					t.Errorf("%s: sees the members of %s", u.name, p.Name)
				}
			}
			for _, path := range lists {
				for _, q := range []string{"", "?project=" + isoB} {
					var items []listed
					if code := u.s.do(t, "GET", path+q, nil, &items); code != http.StatusOK {
						t.Errorf("%s: GET %s%s: %d", u.name, path, q, code)
					}
					for _, it := range items {
						if it.Project == isoB {
							t.Errorf("%s: GET %s%s lists %s/%s", u.name, path, q, it.Project, it.Name)
						}
					}
				}
			}
			// Not an empty filter: their own projects' apps are there.
			var apps []appSummaryJSON
			u.s.do(t, "GET", "/api/v1/apps", nil, &apps)
			if got := names(apps, func(a appSummaryJSON) string { return a.Project + "/" + a.Name }); !slices.Contains(got, isoA+"/web") || !slices.Contains(got, isoTeam+"/web") {
				t.Errorf("%s: apps %v lack their own", u.name, got)
			}
		}
	})

	// Single objects and streams: impersonated, so Kubernetes answers 403.
	b := "/api/v1/projects/" + isoB
	refused := []struct{ method, path string }{
		{"GET", b + "/apps/web"},
		{"GET", b + "/apps/web/pods"},
		{"GET", b + "/apps/web/logs"},
		{"GET", b + "/apps/web/builds"},
		{"GET", b + "/apps/web/metrics"},
		{"GET", b + "/builds/" + e.build},
		{"GET", b + "/builds/" + e.build + "/logs"},
		{"POST", b + "/builds/" + e.build + "/cancel"},
		{"GET", b + "/tasks/" + e.task},
		{"GET", b + "/tasks/" + e.task + "/pods"},
		{"GET", b + "/tasks/" + e.task + "/logs"},
		{"GET", b + "/schedules/backup"},
		{"POST", b + "/apps"},
		{"POST", b + "/tasks"},
		{"POST", b + "/apps/web/restart"},
		{"DELETE", b + "/volumes/data"},
		{"GET", b + "/secret-sets"},
		{"GET", b + "/secret-sets/vault"},
		{"POST", b + "/secret-sets"},
		{"PATCH", b + "/secret-sets/vault"},
		{"DELETE", b + "/secret-sets/vault"},
		{"PUT", b + "/secret-sets/vault/keys/API_KEY"},
		{"DELETE", b + "/secret-sets/vault/keys/API_KEY"},
		{"POST", b + "/secret-sets/vault/keys/API_KEY/reveal"},
		{"POST", b + "/secret-sets/vault/copy"},
	}
	t.Run("console refuses members another project's objects, logs and builds", func(t *testing.T) {
		for _, u := range e.members() {
			for _, req := range refused {
				var body any
				switch {
				case strings.HasSuffix(req.path, "/apps"):
					body = imageApp("intruder", "nginx:1.27")
				case strings.HasSuffix(req.path, "/tasks"):
					body = imageTaskBody("busybox:1.37")
				case strings.HasSuffix(req.path, "/secret-sets"):
					body = map[string]any{"name": "intruder"}
				case strings.HasSuffix(req.path, "/copy"):
					body = map[string]any{"project": isoA}
				case strings.HasSuffix(req.path, "/reveal"):
					body = map[string]any{"password": "a long test password"}
				case req.method == "PUT":
					body = map[string]any{"value": "overwritten"}
				case req.method == "PATCH":
					body = map[string]any{"description": "mine now"}
				}
				var out apiError
				if code := impatient(u.s).do(t, req.method, req.path, body, &out); code != http.StatusForbidden {
					t.Errorf("%s: %s %s: %d %+v, want 403", u.name, req.method, req.path, code, out)
				}
			}
			if !shellRefused(t, u.s, b+"/apps/web/pods/web-b-1/shell") {
				t.Errorf("%s: a shell in iso-b was not refused", u.name)
			}
			if code := u.s.do(t, "GET", "/api/v1/recordings", nil, nil); code != http.StatusForbidden {
				t.Errorf("%s: shell recordings: %d, want 403", u.name, code)
			}
		}
	})
	t.Run("no role reads a secret value through the API, and another project's key names stay hidden", func(t *testing.T) {
		everyone := append(e.members(), e.platform()...)
		for _, u := range everyone {
			for _, p := range []string{isoA, isoB, isoTeam} {
				for _, path := range []string{"/api/v1/projects/" + p + "/secret-sets", "/api/v1/projects/" + p + "/secret-sets/vault",
					"/api/v1/apps?project=" + p, "/api/v1/projects/" + p + "/apps/web"} {
					code, body := u.s.text(t, "GET", path, nil)
					if strings.Contains(body, isoValueA) || strings.Contains(body, isoValueB) {
						t.Errorf("%s: GET %s answers with a secret value", u.name, path)
					}
					if p == isoB && !slices.Contains([]string{store.RoleOwner, store.RoleAdmin}, u.role) && strings.Contains(path, "secret-sets") &&
						(code != http.StatusForbidden || strings.Contains(body, "API_KEY")) {
						t.Errorf("%s: GET %s: %d %s, want 403 without key names", u.name, path, code, body)
					}
				}
			}
		}
		// Members see their project's key names, not values.
		for _, u := range e.members() {
			code, body := u.s.text(t, "GET", "/api/v1/projects/"+isoA+"/secret-sets", nil)
			if code != http.StatusOK || !strings.Contains(body, "API_KEY") {
				t.Errorf("%s: iso-a's secret sets: %d %s", u.name, code, body)
			}
		}
		// The value is still the owner's: nothing above overwrote it.
		var sec corev1.Secret
		if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: isoB, Name: "vault"}, &sec); err != nil || string(sec.Data["API_KEY"]) != isoValueB {
			t.Errorf("iso-b's value changed (err=%v)", err)
		}
	})
	t.Run("members cannot add themselves to a project", func(t *testing.T) {
		for _, u := range e.members() {
			for _, req := range []struct {
				method, path string
				body         any
			}{
				{"POST", b + "/members", map[string]string{"user": u.email, "role": "developer"}},
				{"PUT", b + "/access", map[string]any{"access": "Members", "members": []map[string]string{{"user": u.email, "role": "developer"}}}},
				{"PUT", b + "/access", map[string]any{"access": "Team"}},
				{"POST", "/api/v1/projects/" + isoA + "/members", map[string]string{"user": u.email, "role": "developer"}},
				{"GET", "/api/v1/projects/" + isoA + "/access", nil},
			} {
				if code := u.s.do(t, req.method, req.path, req.body, nil); code != http.StatusForbidden {
					t.Errorf("%s: %s %s: %d, want 403", u.name, req.method, req.path, code)
				}
			}
		}
		var p kwerftv1.Project
		if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: isoB}, &p); err != nil {
			t.Fatal(err)
		}
		if p.Spec.Access != kwerftv1.ProjectAccessMembers || len(p.Spec.Members) != 0 {
			t.Errorf("iso-b changed: %+v", p.Spec)
		}
	})

	// ---- metrics, logs, alerts: read with the console's own identity --------------

	t.Run("metrics are confined to the user's projects", func(t *testing.T) {
		for _, u := range e.members() {
			e.vm.reset()
			var overview struct {
				Scope string `json:"scope"`
			}
			if code := u.s.do(t, "GET", "/api/v1/metrics/overview", nil, &overview); code != http.StatusOK || overview.Scope != "projects" {
				t.Fatalf("%s: overview: %d %+v", u.name, code, overview)
			}
			q := url.Values{"query": {`sum by (namespace) (kwerft:container_memory_working_set_bytes{namespace="iso-b"})`}}
			if code := u.s.do(t, "GET", "/api/v1/metrics/query?"+q.Encode(), nil, nil); code != http.StatusOK {
				t.Fatalf("%s: explore: %d", u.name, code)
			}
			reqs := e.vm.requests()
			if len(reqs) == 0 {
				t.Fatalf("%s: VictoriaMetrics was not asked", u.name)
			}
			for _, r := range reqs {
				confined(t, r, isoA)
				confined(t, r, isoTeam)
				if strings.Contains(r.filters[0], isoB) {
					t.Errorf("%s: filter %q lets iso-b through", u.name, r.filters[0])
				}
			}
		}
	})
	t.Run("log search and tail are confined to the user's projects", func(t *testing.T) {
		e.vl.set(0, "")
		for _, u := range e.members() {
			if code := u.s.do(t, "GET", "/api/v1/logs?query="+url.QueryEscape(`namespace:iso-b OR *`), nil, nil); code != http.StatusOK {
				t.Fatalf("%s: search: %d", u.name, code)
			}
			ns := namespacesOf(e.vl.confinement(t))
			if !slices.Contains(ns, isoA) || !slices.Contains(ns, isoTeam) || slices.Contains(ns, isoB) {
				t.Errorf("%s: log scope %v, want iso-a and iso-team, not iso-b", u.name, ns)
			}
			before := e.vl.requests()
			for _, path := range []string{
				"/api/v1/logs?project=" + isoB,
				"/api/v1/logs?project=" + isoB + "&app=web",
				"/api/v1/logs/tail?project=" + isoB,
			} {
				if code := impatient(u.s).do(t, "GET", path, nil, nil); code != http.StatusNotFound {
					t.Errorf("%s: GET %s: %d, want 404", u.name, path, code)
				}
			}
			if code := u.s.do(t, "GET", "/api/v1/logs?platform=1", nil, nil); code != http.StatusForbidden {
				t.Errorf("%s: platform log search: %d, want 403", u.name, code)
			}
			if after := e.vl.requests(); after != before {
				t.Errorf("%s: refused searches still reached VictoriaLogs (%d requests)", u.name, after-before)
			}
		}
	})
	t.Run("alerts, silences and rules of another project stay hidden", func(t *testing.T) {
		alertA := amAlert(map[string]string{"kwerft_rule": "iso-crash", "namespace": isoA, "app": "web", "severity": "critical"}, "web in iso-a crashes")
		alertB := amAlert(map[string]string{"kwerft_rule": "iso-crash", "namespace": isoB, "app": "web", "severity": "critical"}, "web in iso-b crashes")
		e.am.set(alertA, alertB)
		fpB := alerting.Fingerprint(alertB.Labels)
		e.am.mu.Lock()
		e.am.silences["s-iso-b"] = alerting.Silence{ID: "s-iso-b", Matchers: []alerting.Matcher{{Name: "namespace", Value: isoB, IsEqual: boolPtr(true)}},
			StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), CreatedBy: "owner@example.com", Comment: "b"}
		e.am.mu.Unlock()
		ctx := context.Background()
		rule := &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "iso-b-restarts"},
			Spec: kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Severity: "warning", Scope: kwerftv1.AlertScope{Apps: []string{isoB + "/web"}}}}
		if err := cluster.admin.Create(ctx, rule); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { deleteRules(t, "iso-b-restarts", "iso-dana-b") })
		eventually(t, func() error {
			if _, ok := ruleNamed(t, e.owner, "iso-b-restarts"); !ok {
				return fmt.Errorf("rule not in the cache yet")
			}
			return nil
		})

		for _, u := range e.members() {
			alerts := alertsOf(t, u.s, "")
			if len(alerts) != 1 || alerts[0].Project != isoA {
				t.Errorf("%s: alerts %+v, want iso-a's only", u.name, alerts)
			}
			var silences []silenceJSON
			u.s.do(t, "GET", "/api/v1/alerts/silences", nil, &silences)
			for _, s := range silences {
				if s.ID == "s-iso-b" {
					t.Errorf("%s: sees iso-b's silence", u.name)
				}
			}
			if _, ok := ruleNamed(t, u.s, "iso-b-restarts"); ok {
				t.Errorf("%s: sees a rule about iso-b", u.name)
			}
			for _, req := range []struct {
				method, path string
				body         any
				want         int
			}{
				{"POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": fpB, "duration": "1h"}, http.StatusNotFound},
				{"POST", "/api/v1/alerts/silences", map[string]any{"matchers": []map[string]any{{"name": "namespace", "value": isoB}}, "duration": "1h"}, http.StatusForbidden},
				{"DELETE", "/api/v1/alerts/silences/s-iso-b", nil, http.StatusNotFound},
				{"POST", "/api/v1/alerts/rules", map[string]any{"name": "iso-dana-b", "condition": "Restarts", "scope": map[string]any{"projects": []string{isoB}}}, http.StatusForbidden},
				{"POST", "/api/v1/alerts/rules", map[string]any{"name": "iso-dana-b", "condition": "Restarts"}, http.StatusForbidden},
				{"PUT", "/api/v1/alerts/rules/iso-b-restarts", map[string]any{"condition": "Restarts", "scope": map[string]any{"projects": []string{isoA}}}, http.StatusForbidden},
				{"DELETE", "/api/v1/alerts/rules/iso-b-restarts", nil, http.StatusForbidden},
			} {
				if code := u.s.do(t, req.method, req.path, req.body, nil); code != req.want {
					t.Errorf("%s: %s %s: %d, want %d", u.name, req.method, req.path, code, req.want)
				}
			}
		}
		// dana works in iso-a: she silences its alerts and writes its rules.
		if code := e.dana.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": alerting.Fingerprint(alertA.Labels), "duration": "1h"}, nil); code != http.StatusCreated {
			t.Errorf("dana silences an iso-a alert: %d", code)
		}
		if code := e.dana.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"name": "iso-dana-b", "condition": "Restarts", "scope": map[string]any{"projects": []string{isoA}}}, nil); code != http.StatusCreated {
			t.Errorf("dana writes a rule for iso-a: %d", code)
		}
		// Owners and admins see every project's alerts.
		for _, u := range e.platform() {
			if got := alertsOf(t, u.s, ""); len(got) != 2 {
				t.Errorf("%s: %d alerts, want both", u.name, len(got))
			}
			if _, ok := ruleNamed(t, u.s, "iso-b-restarts"); !ok {
				t.Errorf("%s: does not see the iso-b rule", u.name)
			}
		}
	})

	// ---- owners and admins ---------------------------------------------------------

	t.Run("owners and admins reach every project through the console", func(t *testing.T) {
		for _, u := range e.platform() {
			var projects []projectJSON
			u.s.do(t, "GET", "/api/v1/projects", nil, &projects)
			got := names(projects, func(p projectJSON) string { return p.Name })
			for _, p := range []string{isoA, isoB, isoTeam} {
				if !slices.Contains(got, p) {
					t.Errorf("%s: projects %v lack %s", u.name, got, p)
				}
			}
			for _, path := range lists {
				var items []listed
				u.s.do(t, "GET", path+"?project="+isoB, nil, &items)
				if len(items) == 0 {
					t.Errorf("%s: GET %s?project=iso-b is empty", u.name, path)
				}
			}
			if code := u.s.do(t, "GET", b+"/apps/web", nil, nil); code != http.StatusOK {
				t.Errorf("%s: GET an iso-b app: %d", u.name, code)
			}
			var access projectAccessJSON
			if code := u.s.do(t, "GET", "/api/v1/projects/"+isoA+"/access", nil, &access); code != http.StatusOK || len(access.Members) != 2 {
				t.Errorf("%s: iso-a access: %d %+v", u.name, code, access)
			}
			e.vm.reset()
			var overview struct {
				Scope string `json:"scope"`
			}
			if code := u.s.do(t, "GET", "/api/v1/metrics/overview", nil, &overview); code != http.StatusOK || overview.Scope != "all" {
				t.Errorf("%s: overview: %d %+v", u.name, code, overview)
			}
		}
	})

	t.Run("a token limited to iso-a lists only iso-a", func(t *testing.T) { projectTokenListsOnlyItsProjects(t, e) })
}

func shellRefused(t *testing.T, s *session, path string) bool {
	t.Helper()
	d := websocket.Dialer{Jar: s.client.Jar, HandshakeTimeout: 5 * time.Second}
	conn, res, err := d.Dial("ws"+strings.TrimPrefix(s.url, "http")+path, http.Header{"Origin": {s.url}})
	if err != nil {
		return res != nil && res.StatusCode == http.StatusForbidden
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return false
		}
		if kind != websocket.TextMessage {
			continue
		}
		var ev map[string]any
		_ = json.Unmarshal(data, &ev)
		switch ev["type"] {
		case "error":
			return strings.Contains(fmt.Sprint(ev["message"]), "does not allow")
		case "started", "exit":
			return false
		}
	}
}

// projectTokenListsOnlyItsProjects (a subtest of TestProjectIsolation, which
// owns the fixtures): a token limited to one project sees that project in the
// lists across
// projects, even though its user reaches more (dana: iso-a and iso-team).
func projectTokenListsOnlyItsProjects(t *testing.T, e *isolationEnv) {
	var created struct {
		Token string `json:"token"`
	}
	if code := e.dana.do(t, "POST", "/api/v1/account/tokens", map[string]any{"name": "iso-a ci", "role": "developer", "projects": []string{isoA}}, &created); code != http.StatusCreated || created.Token == "" {
		t.Fatalf("create token: %d", code)
	}
	get := func(path string, out any) int {
		t.Helper()
		req, err := http.NewRequest("GET", e.dana.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+created.Token)
		res, err := http.DefaultClient.Do(req) // no cookies: the token alone
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if out != nil {
			_ = json.NewDecoder(res.Body).Decode(out)
		}
		return res.StatusCode
	}
	var apps []appSummaryJSON
	if code := get("/api/v1/apps", &apps); code != http.StatusOK {
		t.Fatalf("apps: %d", code)
	}
	for _, a := range apps {
		if a.Project != isoA {
			t.Errorf("the token sees %s/%s", a.Project, a.Name)
		}
	}
	if len(apps) == 0 {
		t.Error("the token sees none of iso-a's apps")
	}
	var projects []map[string]any
	if code := get("/api/v1/projects", &projects); code != http.StatusOK {
		t.Fatalf("projects: %d", code)
	}
	for _, p := range projects {
		if p["name"] != isoA {
			t.Errorf("the token sees project %v", p["name"])
		}
	}
	// The session behind it still sees both of dana's projects.
	var all []appSummaryJSON
	if code := e.dana.do(t, "GET", "/api/v1/apps", nil, &all); code != http.StatusOK || len(all) < 2 {
		t.Errorf("dana's own list: %d, %d apps", code, len(all))
	}
}
