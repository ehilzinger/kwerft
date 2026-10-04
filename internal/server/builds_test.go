package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

func gitApp(name, repo string) map[string]any {
	return map[string]any{"name": name, "spec": map[string]any{
		"source": map[string]any{"git": map[string]any{"repository": repo}},
	}}
}

func sha(c byte) string { return strings.Repeat(string(c), 40) }

// makeBuild stands in for a webhook or "Build now" (W3) and the Build
// reconciler (W2): it creates a Build for the App and writes its status.
func makeBuild(t *testing.T, project, app string, commit string, status kwerftv1.BuildStatus) *kwerftv1.Build {
	t.Helper()
	ctx := context.Background()
	var a kwerftv1.App
	if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: project, Name: app}, &a); err != nil {
		t.Fatal(err)
	}
	b, err := builds.New(&a, builds.Request{
		Commit:  builds.Commit{SHA: commit, Branch: "main", Message: "Add order export\n\nLonger body.", Author: "Aiko Tanaka"},
		Trigger: "push", RequestedBy: "aiko", Deploy: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.admin.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.Status = status
	if err := cluster.admin.Status().Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBuildsListGetAndCancel(t *testing.T) {
	c := newConsole(t)
	c.project(t, "ci")
	c.project(t, "ci-other")
	ctx := context.Background()
	if code := c.dev.do(t, "POST", "/api/v1/projects/ci/apps", gitApp("api", "https://github.com/acme/api.git"), nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/ci/apps", gitApp("web", "https://github.com/acme/web.git"), nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/ci-other/apps", gitApp("api", "https://github.com/acme/api.git"), nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}

	start := metav1.NewTime(time.Now().Add(-3 * time.Minute).Truncate(time.Second))
	done := metav1.NewTime(start.Add(91 * time.Second))
	first := makeBuild(t, "ci", "api", sha('a'), kwerftv1.BuildStatus{
		Phase: kwerftv1.BuildSucceeded, Number: 1, Image: builds.ImageRef("ci", "api", sha('a')), Digest: "sha256:" + sha('d'),
		StartTime: &start, CompletionTime: &done,
	})
	second := makeBuild(t, "ci", "api", sha('b'), kwerftv1.BuildStatus{Phase: kwerftv1.BuildRunning, Number: 2, StartTime: &start, Message: "Building"})
	makeBuild(t, "ci", "web", sha('c'), kwerftv1.BuildStatus{Phase: kwerftv1.BuildPending})
	makeBuild(t, "ci-other", "api", sha('e'), kwerftv1.BuildStatus{Phase: kwerftv1.BuildPending})

	// The App reconciler (W2) records which build a revision ran.
	var app kwerftv1.App
	if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "ci", Name: "api"}, &app); err != nil {
		t.Fatal(err)
	}
	app.Status.History = []kwerftv1.AppRevision{{Number: 4, Image: first.Status.Image, Generation: 1, Build: first.Name, Commit: sha('a'), Time: done}}
	if err := cluster.admin.Status().Update(ctx, &app); err != nil {
		t.Fatal(err)
	}

	// Every role reads builds; the list holds this App's, newest first.
	var list []buildJSON
	if code := c.viewer.do(t, "GET", "/api/v1/projects/ci/apps/api/builds", nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if got := names(list, func(b buildJSON) string { return b.Name }); !slices.Equal(got, []string{second.Name, first.Name}) {
		t.Fatalf("builds = %v, want %s, %s", got, second.Name, first.Name)
	}
	b1 := list[1]
	if b1.Number != 1 || b1.Phase != "succeeded" || b1.Message != "Add order export" || b1.Author != "Aiko Tanaka" ||
		b1.Trigger != "push" || !b1.Deploy || b1.DeployedRevision != 4 || !b1.Current || b1.Source.Builder != "dockerfile" ||
		b1.Source.Repository != "https://github.com/acme/api.git" || b1.DurationSeconds == nil || *b1.DurationSeconds != 91 {
		t.Errorf("first build = %+v", b1)
	}
	if b2 := list[0]; b2.Phase != "running" || b2.StatusMessage != "Building" || b2.DeployedRevision != 0 || b2.DurationSeconds == nil || *b2.DurationSeconds < 170 {
		t.Errorf("second build = %+v", b2)
	}
	if code := c.viewer.do(t, "GET", "/api/v1/projects/ci/apps/api/builds?limit=1", nil, &list); code != http.StatusOK || len(list) != 1 {
		t.Errorf("limit 1: %d, %d builds", code, len(list))
	}
	var e apiError
	if code := c.viewer.do(t, "GET", "/api/v1/projects/ci/apps/nope/builds", nil, &e); code != http.StatusNotFound {
		t.Errorf("builds of a missing app: %d %+v", code, e)
	}

	var one buildJSON
	if code := c.viewer.do(t, "GET", "/api/v1/projects/ci/builds/"+first.Name, nil, &one); code != http.StatusOK || one.Name != first.Name || one.Image == "" || one.DeployedRevision != 4 {
		t.Errorf("get: %d %+v", code, one)
	}
	if code := c.viewer.do(t, "GET", "/api/v1/projects/ci-other/builds/"+first.Name, nil, &e); code != http.StatusNotFound {
		t.Errorf("a build under another project: %d", code)
	}

	// The App JSON carries the newest build and, in the summary, the Git source.
	var detail struct {
		kwerftv1.App
		LatestBuild *buildJSON `json:"latestBuild"`
	}
	if code := c.viewer.do(t, "GET", "/api/v1/projects/ci/apps/api", nil, &detail); code != http.StatusOK || detail.LatestBuild == nil || detail.LatestBuild.Name != second.Name {
		t.Errorf("app latestBuild: %d %+v", code, detail.LatestBuild)
	}
	if detail.Status.History[0].Build != first.Name || detail.Status.History[0].Commit != sha('a') {
		t.Errorf("revision = %+v", detail.Status.History[0])
	}
	var apps []appSummaryJSON
	c.viewer.do(t, "GET", "/api/v1/apps?project=ci", nil, &apps)
	if len(apps) != 2 || apps[0].Source.Builder != "dockerfile" || apps[0].Source.Dockerfile != "Dockerfile" || apps[0].Source.Branch != "main" ||
		apps[0].Source.AutoDeploy == nil || !*apps[0].Source.AutoDeploy {
		t.Errorf("summary source = %+v", apps)
	}

	// Cancel: a write, so not for viewers; finished builds refuse.
	if code := c.viewer.do(t, "POST", "/api/v1/projects/ci/builds/"+second.Name+"/cancel", nil, &e); code != http.StatusForbidden {
		t.Errorf("viewer cancels: %d", code)
	}
	if code := c.owner.do(t, "POST", "/api/v1/projects/ci/builds/"+first.Name+"/cancel", nil, &e); code != http.StatusConflict {
		t.Errorf("cancel a finished build: %d", code)
	}
	var cancelled buildJSON
	if code := c.owner.do(t, "POST", "/api/v1/projects/ci/builds/"+second.Name+"/cancel", nil, &cancelled); code != http.StatusOK || !cancelled.CancelRequested {
		t.Errorf("cancel: %d %+v", code, cancelled)
	}
	var b kwerftv1.Build
	if err := cluster.admin.Get(ctx, client.ObjectKeyFromObject(second), &b); err != nil {
		t.Fatal(err)
	}
	if b.Annotations[kwerftv1.AnnotationCancelRequested] != "owner@example.com" {
		t.Errorf("annotations = %v", b.Annotations)
	}

	entries, err := c.store.RecentAudit(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := names(entries, func(e store.AuditEntry) string { return e.Actor + " " + e.Action + " " + e.Target + " " + e.Detail })
	for _, want := range []string{
		"owner@example.com build.cancel ci/" + second.Name + " app api #2",
		"viewer@example.com build.cancel.denied ci/" + second.Name + " forbidden by Kubernetes RBAC",
	} {
		if !slices.Contains(seen, want) {
			t.Errorf("audit lacks %q: %v", want, seen)
		}
	}
}

func TestGitSourcesAreValidated(t *testing.T) {
	c := newConsole(t)
	c.project(t, "gitval")
	c.project(t, "gitval-other")
	ctx := context.Background()
	for _, conn := range []*kwerftv1.GitConnection{
		{ObjectMeta: metav1.ObjectMeta{Name: "github"}, Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitHub, URL: "https://github.com", Auth: kwerftv1.GitAuthToken}},
		{ObjectMeta: metav1.ObjectMeta{Name: "gitlab-other"}, Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitLab, URL: "https://gitlab.com", Auth: kwerftv1.GitAuthToken, Projects: []string{"gitval-other"}}},
	} {
		if err := cluster.admin.Create(ctx, conn); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	src := func(git map[string]any) map[string]any {
		return map[string]any{"name": "svc", "spec": map[string]any{"source": map[string]any{"git": git}}}
	}
	for _, tc := range []struct {
		git   map[string]any
		field string
	}{
		{map[string]any{"repository": "http://github.com/acme/api"}, "spec.source.git.repository"},
		{map[string]any{"repository": "https://token:x@github.com/acme/api"}, "spec.source.git.repository"},
		{map[string]any{"repository": "github.com/acme/api"}, "spec.source.git.repository"},
		{map[string]any{"repository": "https://github.com/acme/../api"}, "spec.source.git.repository"},
		{map[string]any{"repository": "git@github.com:acme/api.git", "branch": "feature..x"}, "spec.source.git.branch"},
		{map[string]any{"repository": "git@github.com:acme/api.git", "branch": "-main"}, "spec.source.git.branch"},
		{map[string]any{"repository": "git@github.com:acme/api.git", "path": "../secrets"}, "spec.source.git.path"},
		{map[string]any{"repository": "git@github.com:acme/api.git", "dockerfile": "docker/"}, "spec.source.git.dockerfile"},
		{map[string]any{"repository": "git@github.com:acme/api.git", "builder": "kaniko"}, "spec.source.git.builder"},
		{map[string]any{"repository": "git@github.com:acme/api.git", "connection": "No_Such"}, "spec.source.git.connection"},
		{map[string]any{"repository": "git@github.com:acme/api.git", "connection": "missing"}, "spec.source.git.connection"},
		{map[string]any{"repository": "https://gitlab.com/acme/api", "connection": "gitlab-other"}, "spec.source.git.connection"},
	} {
		var e apiError
		if code := c.dev.do(t, "POST", "/api/v1/projects/gitval/apps", src(tc.git), &e); code != http.StatusUnprocessableEntity || e.Field != tc.field {
			t.Errorf("%v: %d %+v, want 422 on %s", tc.git, code, e, tc.field)
		}
	}
	for i, git := range []map[string]any{
		{"repository": "https://github.com/acme/api.git", "connection": "github", "branch": "release/2.x", "path": "/services/api", "builder": "railpack"},
		{"repository": "git@gitlab.com:acme/group/api.git", "dockerfile": "docker/Dockerfile.prod"},
		{"repository": "ssh://git@git.example.com:2222/acme/api.git"},
		{"repository": "https://gitlab.com/acme/api", "connection": "gitlab-other"},
	} {
		project := "gitval"
		if git["connection"] == "gitlab-other" {
			project = "gitval-other"
		}
		body := src(git)
		body["name"] = fmt.Sprintf("ok-%d", i)
		var e apiError
		if code := c.dev.do(t, "POST", "/api/v1/projects/"+project+"/apps", body, &e); code != http.StatusCreated {
			t.Errorf("%v: %d %+v, want 201", git, code, e)
		}
	}

	// Updates are checked the same way; an unchanged connection is not looked up again.
	var app kwerftv1.App
	if code := c.dev.do(t, "GET", "/api/v1/projects/gitval/apps/ok-0", nil, &app); code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	app.Spec.Source.Git.Connection = "gitlab-other"
	var e apiError
	if code := c.dev.do(t, "PUT", "/api/v1/projects/gitval/apps/ok-0", map[string]any{"spec": app.Spec, "generation": app.Generation}, &e); code != http.StatusUnprocessableEntity || e.Field != "spec.source.git.connection" {
		t.Errorf("update to a connection of another project: %d %+v", code, e)
	}
	app.Spec.Source.Git.Connection = "github"
	app.Spec.Source.Git.PinnedImage = ""
	app.Spec.Source.Git.Branch = "main"
	if code := c.dev.do(t, "PUT", "/api/v1/projects/gitval/apps/ok-0", map[string]any{"spec": app.Spec, "generation": app.Generation}, &e); code != http.StatusOK {
		t.Errorf("update: %d %+v", code, e)
	}
}

// ---- the build log, against fakes ------------------------------------------------

type fakeBuilds struct {
	builds map[string]*kwerftv1.Build // project/name
	denied bool
}

func (f *fakeBuilds) get(_ context.Context, _ *principal, project, name string) (*kwerftv1.Build, error) {
	if f.denied {
		return nil, apierrors.NewForbidden(schema.GroupResource{Group: "kwerft.dev", Resource: "builds"}, name, fmt.Errorf("no"))
	}
	if b, ok := f.builds[project+"/"+name]; ok {
		return b.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "kwerft.dev", Resource: "builds"}, name)
}

func buildPod(name, project, build string, running bool) corev1.Pod {
	p := appPod(name, "", time.Now().Add(-time.Minute), running)
	p.Namespace = builds.Namespace
	p.Labels = map[string]string{builds.LabelBuild: build, controllers.LabelProject: project}
	if !running {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}
	}
	return p
}

func newBuildLogEnv(t *testing.T, fb *fakeBuilds, fake *fakePods) (*podEnv, *buildsAPI) {
	t.Helper()
	var api *buildsAPI
	e := newEnv(t, func(c *Config) {
		c.buildsHook = func(b *buildsAPI) {
			b.getBuild = fb.get
			b.ownPods = func() (podBackend, error) { return fakeBackend{fake}, nil }
			b.logs = fastLogLimits()
			api = b
		}
	})
	e.completeSetup(t)
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for api.running.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := api.running.Load(); n > 0 {
			t.Errorf("%d build log streams still running", n)
		}
	})
	return &podEnv{env: e, fake: fake}, api
}

func TestBuildLogFollowsTheBuildPod(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	fb := &fakeBuilds{builds: map[string]*kwerftv1.Build{
		"shop/api-4f2c1ab-x": {ObjectMeta: metav1.ObjectMeta{Name: "api-4f2c1ab-x", Namespace: "shop"}, Status: kwerftv1.BuildStatus{Phase: kwerftv1.BuildRunning}},
	}}
	fake := newFakePods(
		buildPod("api-4f2c1ab-x-job-abcde", "shop", "api-4f2c1ab-x", true),
		// The same build name in another project: never part of this log.
		buildPod("api-4f2c1ab-x-job-zzzzz", "other", "api-4f2c1ab-x", true),
	)
	fake.backlog["api-4f2c1ab-x-job-abcde"] = []string{ts(base, 0) + " #1 [internal] load build definition from Dockerfile"}
	fake.backlog["api-4f2c1ab-x-job-zzzzz"] = []string{ts(base, 0) + " secret other project"}
	pe, _ := newBuildLogEnv(t, fb, fake)

	events, cancel, res := pe.stream(t, "/api/v1/projects/shop/builds/api-4f2c1ab-x/logs?follow=1&tail=5000")
	defer cancel()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", res.StatusCode)
	}
	start := next(t, events, "start")
	if pods := start.data["pods"].([]any); len(pods) != 1 || pods[0] != "api-4f2c1ab-x-job-abcde" || start.data["follow"] != true {
		t.Errorf("start = %v", start.data)
	}
	if ln := next(t, events, "line"); ln.data["text"] != "#1 [internal] load build definition from Dockerfile" {
		t.Errorf("backlog line = %v", ln.data)
	}
	fake.write(t, "api-4f2c1ab-x-job-abcde", ts(base, 5000)+" #2 DONE 0.1s")
	if ln := next(t, events, "line"); ln.data["text"] != "#2 DONE 0.1s" {
		t.Errorf("live line = %v", ln.data)
	}
	for _, call := range fake.followCalls() {
		if call.Container != "app" {
			t.Errorf("follow call = %+v", call)
		}
	}
}

func TestBuildLogOfAFinishedBuild(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	done := func(name string) *kwerftv1.Build {
		return &kwerftv1.Build{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"}, Status: kwerftv1.BuildStatus{Phase: kwerftv1.BuildFailed}}
	}
	fb := &fakeBuilds{builds: map[string]*kwerftv1.Build{"shop/kept": done("kept"), "shop/pruned": done("pruned")}}
	fake := newFakePods(buildPod("kept-job-abcde", "shop", "kept", false))
	fake.backlog["kept-job-abcde"] = []string{ts(base, 0) + " ERROR: failed to solve"}
	pe, _ := newBuildLogEnv(t, fb, fake)

	// A finished build is read once, even when the console asks to follow.
	events, cancel, _ := pe.stream(t, "/api/v1/projects/shop/builds/kept/logs?follow=1")
	if ln := next(t, events, "line"); ln.data["text"] != "ERROR: failed to solve" {
		t.Errorf("line = %v", ln.data)
	}
	if end := next(t, events, "end"); end.data["reason"] != "complete" {
		t.Errorf("end = %v", end.data)
	}
	cancel()
	if n := len(fake.followCalls()); n != 0 {
		t.Errorf("%d follow calls for a finished build", n)
	}

	// Its pod gone, the stream says so instead of showing an empty log.
	events, cancel, _ = pe.stream(t, "/api/v1/projects/shop/builds/pruned/logs?follow=1")
	defer cancel()
	if end := next(t, events, "end"); end.data["reason"] != "gone" || !strings.Contains(end.data["message"].(string), "pod has been removed") {
		t.Errorf("end = %v", end.data)
	}
}

func TestBuildLogNeedsTheBuild(t *testing.T) {
	fb := &fakeBuilds{builds: map[string]*kwerftv1.Build{}}
	fake := newFakePods(buildPod("ghost-job-abcde", "shop", "ghost", true))
	pe, _ := newBuildLogEnv(t, fb, fake)

	_, cancel, res := pe.stream(t, "/api/v1/projects/shop/builds/ghost/logs")
	cancel()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("log of a build the user cannot see: %d, want 404", res.StatusCode)
	}
	fb.denied = true
	_, cancel, res = pe.stream(t, "/api/v1/projects/shop/builds/ghost/logs")
	cancel()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("forbidden get: %d, want 403", res.StatusCode)
	}
	if n := fake.opened.Load(); n != 0 {
		t.Errorf("%d pod logs opened without the Build", n)
	}
	entries, err := pe.store.RecentAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e store.AuditEntry) bool { return e.Action == "build.logs.denied" && e.Target == "shop/ghost" }) {
		t.Errorf("no build.logs.denied audit entry: %+v", entries)
	}
}

// The real path: the console reads kwerft-builds with its own identity.
func TestBuildLogOfAPrunedBuildAgainstTheCluster(t *testing.T) {
	c := newConsole(t)
	c.project(t, "ci-logs")
	if code := c.dev.do(t, "POST", "/api/v1/projects/ci-logs/apps", gitApp("api", "https://github.com/acme/api.git"), nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	b := makeBuild(t, "ci-logs", "api", sha('f'), kwerftv1.BuildStatus{Phase: kwerftv1.BuildSucceeded, Number: 1})
	// A finished build without a pod: the stream ends at once.
	res, err := c.viewer.client.Get(c.viewer.url + "/api/v1/projects/ci-logs/builds/" + b.Name + "/logs?follow=1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"reason":"gone"`) {
		t.Errorf("log of a pruned build: %d %q", res.StatusCode, body)
	}
}
