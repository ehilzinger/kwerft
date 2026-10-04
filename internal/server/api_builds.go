package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/logs"
)

// Builds of Git apps (docs/phase2.md): list an App's builds, show one, stream
// its log, cancel it. Starting builds ("Build now") and Git connections are
// in api_git.go.
//
// Every read and write reaches Kubernetes as the signed-in user, like the rest
// of the workload API, with one exception: the build log. The build pod runs
// in the kwerft-builds namespace, which no console role may read (it also
// holds the Git credentials). The console reads that pod's log with its own
// identity (kube.Impersonator.Self), and only after the user's impersonated
// get of the Build succeeded: whoever may see a build may see its log, and
// nobody reaches anything else in that namespace through this endpoint.

// buildJSON is a Build as the console shows it.
type buildJSON struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	// Number is #1, #2, ... per App; 0 until the reconciler numbers it.
	Number int64           `json:"number"`
	App    string          `json:"app"`
	Source buildSourceJSON `json:"source"`
	Commit string          `json:"commit"`
	Branch string          `json:"branch,omitempty"`
	// Message is the commit's subject line. StatusMessage says why a build
	// failed or waits (docs/phase2.md lists both as "message").
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
	Trigger     string `json:"trigger"` // push | manual | pull-request
	RequestedBy string `json:"requestedBy,omitempty"`
	PullRequest int64  `json:"pullRequest,omitempty"`
	Deploy      bool   `json:"deploy"`
	// Phase is pending | running | succeeded | failed | cancelled.
	Phase         string `json:"phase"`
	StatusMessage string `json:"statusMessage,omitempty"`
	// CancelRequested: someone asked to cancel it and the reconciler has not
	// stopped it yet.
	CancelRequested bool       `json:"cancelRequested,omitempty"`
	Image           string     `json:"image,omitempty"`
	Digest          string     `json:"digest,omitempty"`
	Created         time.Time  `json:"created"`
	Started         *time.Time `json:"started,omitempty"`
	Finished        *time.Time `json:"finished,omitempty"`
	// DurationSeconds runs from start to finish, or to now while running.
	DurationSeconds *int64 `json:"durationSeconds,omitempty"`
	// DeployedRevision is the newest App revision that ran this build's
	// image; Current says it is the revision running now.
	DeployedRevision int64 `json:"deployedRevision,omitempty"`
	Current          bool  `json:"current,omitempty"`
}

type buildSourceJSON struct {
	Repository string `json:"repository"`
	Path       string `json:"path,omitempty"`
	Builder    string `json:"builder"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Connection string `json:"connection,omitempty"`
}

func buildFinished(p kwerftv1.BuildPhase) bool {
	return p == kwerftv1.BuildSucceeded || p == kwerftv1.BuildFailed || p == kwerftv1.BuildCancelled
}

// buildSummary condenses a Build; app (may be nil) supplies the revision that
// deployed it.
func buildSummary(b *kwerftv1.Build, app *kwerftv1.App, now time.Time) buildJSON {
	out := buildJSON{
		Name: b.Name, Project: b.Namespace, Number: b.Status.Number, App: b.Spec.App,
		Source: buildSourceJSON{
			Repository: b.Spec.Source.Repository, Path: b.Spec.Source.Path, Builder: b.Spec.Source.Builder,
			Dockerfile: b.Spec.Source.Dockerfile, Connection: b.Spec.Source.Connection,
		},
		Commit: b.Spec.Commit, Branch: b.Spec.Branch, Message: b.Spec.Message, Author: b.Spec.Author,
		Trigger: b.Spec.Trigger, RequestedBy: b.Spec.RequestedBy, PullRequest: b.Spec.PullRequest, Deploy: b.Spec.Deploy,
		Phase: strings.ToLower(string(b.Status.Phase)), StatusMessage: b.Status.Message,
		Image: b.Status.Image, Digest: b.Status.Digest, Created: b.CreationTimestamp.UTC(),
		Started: timePtr(b.Status.StartTime), Finished: timePtr(b.Status.CompletionTime),
	}
	if out.Phase == "" {
		out.Phase = "pending"
	}
	out.CancelRequested = b.Annotations[kwerftv1.AnnotationCancelRequested] != "" && !buildFinished(b.Status.Phase)
	if out.Started != nil {
		end := now
		if out.Finished != nil {
			end = *out.Finished
		}
		d := int64(max(end.Sub(*out.Started), 0) / time.Second)
		out.DurationSeconds = &d
	}
	if app != nil {
		for i, rev := range app.Status.History { // newest first
			if rev.Build == b.Name {
				out.DeployedRevision, out.Current = rev.Number, i == 0
				break
			}
		}
	}
	return out
}

func buildNotFound(project, name string) string {
	return fmt.Sprintf("Build %q in project %q not found.", name, project)
}

// appBuilds lists an App's builds as the user, newest first.
func appBuilds(ctx context.Context, c client.Client, project, app string) ([]kwerftv1.Build, error) {
	var list kwerftv1.BuildList
	if err := c.List(ctx, &list, client.InNamespace(project)); err != nil {
		return nil, err
	}
	// By spec.app rather than a label: a Build made with kubectl may lack it.
	out := slices.DeleteFunc(list.Items, func(b kwerftv1.Build) bool { return b.Spec.App != app })
	slices.SortFunc(out, func(x, y kwerftv1.Build) int {
		return cmp.Or(
			y.CreationTimestamp.Compare(x.CreationTimestamp.Time),
			cmp.Compare(y.Status.Number, x.Status.Number),
			strings.Compare(y.Name, x.Name),
		)
	})
	return out, nil
}

// latestBuild is the App's newest build for the App JSON, or nil when there
// is none or the user may not list builds (the app itself still shows).
func (a *api) latestBuild(ctx context.Context, c client.Client, app *kwerftv1.App) *buildJSON {
	list, err := appBuilds(ctx, c, app.Namespace, app.Name)
	if err != nil || len(list) == 0 {
		return nil
	}
	b := buildSummary(&list[0], app, a.now())
	return &b
}

// ---- routes ------------------------------------------------------------------

// buildsAPI holds what the build log stream needs besides the api.
type buildsAPI struct {
	*api
	// getBuild reads a Build as the user; ownPods reaches the build pods with
	// the console's own identity; both in the cluster of the context (the
	// project's). Tests swap both for fakes.
	getBuild func(ctx context.Context, pr *principal, project, name string) (*kwerftv1.Build, error)
	ownPods  func(ctx context.Context) (podBackend, error)
	logs     logLimits
	streams  *slots
	// running counts open streams, so tests can wait for cleanup.
	running atomic.Int32
}

func (a *api) registerBuilds(mux *http.ServeMux) {
	b := &buildsAPI{api: a, logs: defaultLogLimits, streams: newSlots(6, 200)}
	b.getBuild = b.kubeBuild
	b.ownPods = b.kubeOwnPods
	if a.cfg.buildsHook != nil {
		a.cfg.buildsHook(b)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }

	mux.HandleFunc("GET /api/v1/projects/{project}/apps/{app}/builds", read(a.buildList))
	mux.HandleFunc("GET /api/v1/projects/{project}/builds/{build}", read(a.buildGet))
	mux.HandleFunc("POST /api/v1/projects/{project}/builds/{build}/cancel", write(a.buildCancel))
	// A long-lived read with application output: refused from other sites
	// like a write, as the pod log streams are.
	mux.HandleFunc("GET /api/v1/projects/{project}/builds/{build}/logs", a.sameOrigin(a.requireUser(b.requireBackend(b.buildLogs))))
}

func (b *buildsAPI) requireBackend(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.cfg.Kube == nil && b.cfg.buildsHook == nil {
			writeError(w, http.StatusServiceUnavailable, "This console is not connected to a Kubernetes cluster.")
			return
		}
		next(w, r)
	}
}

func (b *buildsAPI) kubeBuild(ctx context.Context, pr *principal, project, name string) (*kwerftv1.Build, error) {
	c, err := b.conn(ctx).kube.For(pr.user.Email, pr.user.Role)
	if err != nil {
		return nil, err
	}
	var build kwerftv1.Build
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &build); err != nil {
		return nil, err
	}
	return &build, nil
}

func (b *buildsAPI) kubeOwnPods(ctx context.Context) (podBackend, error) {
	cs, err := b.conn(ctx).kube.Self()
	if err != nil {
		return nil, err
	}
	// Only listPods and logs are used, which need nothing but the clientset.
	return &kubePods{cs: cs}, nil
}

// ---- list, get, cancel -------------------------------------------------------

func (a *api) buildList(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			invalid(w, "limit", "Enter a number of builds, 1 or more.")
			return
		}
		limit = min(n, 500)
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	var app kwerftv1.App
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &app); err != nil {
		a.kubeError(w, r, p, "build.list", target, appNotFound(project, name), err)
		return
	}
	list, err := appBuilds(ctx, c, project, name)
	if err != nil {
		a.kubeError(w, r, p, "build.list", target, appNotFound(project, name), err)
		return
	}
	now := a.now()
	out := make([]buildJSON, 0, min(len(list), limit))
	for i := range list[:min(len(list), limit)] {
		out = append(out, buildSummary(&list[i], &app, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) buildGet(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("build")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var build kwerftv1.Build
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &build); err != nil {
		a.kubeError(w, r, p, "build.get", appTarget(project, name), buildNotFound(project, name), err)
		return
	}
	writeJSON(w, http.StatusOK, buildSummary(&build, a.buildApp(ctx, c, &build), a.now()))
}

// buildApp is the Build's App, or nil (deleted, or not readable).
func (a *api) buildApp(ctx context.Context, c client.Client, b *kwerftv1.Build) *kwerftv1.App {
	var app kwerftv1.App
	if err := c.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.App}, &app); err != nil {
		return nil
	}
	return &app
}

// buildCancel asks the Build reconciler to stop an unfinished build (see
// kwerftv1.AnnotationCancelRequested): it deletes the Job and marks the build
// Cancelled. The Build and its record stay.
func (a *api) buildCancel(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("build")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	var build kwerftv1.Build
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &build); err != nil {
		a.kubeError(w, r, p, "build.cancel", target, buildNotFound(project, name), err)
		return
	}
	if buildFinished(build.Status.Phase) {
		writeError(w, http.StatusConflict, "This build has already finished.")
		return
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{kwerftv1.AnnotationCancelRequested: p.user.Email},
	}})
	if err := c.Patch(ctx, &build, client.RawPatch(types.MergePatchType, patch)); err != nil {
		a.kubeError(w, r, p, "build.cancel", target, buildNotFound(project, name), err)
		return
	}
	detail := "app " + build.Spec.App
	if build.Status.Number > 0 {
		detail += fmt.Sprintf(" #%d", build.Status.Number)
	}
	a.audit(r, p.user.Email, "build.cancel", target, detail)
	writeJSON(w, http.StatusOK, buildSummary(&build, a.buildApp(ctx, c, &build), a.now()))
}

// ---- log -------------------------------------------------------------------------

// buildPodSelector picks a Build's pods in kwerft-builds. Build names are only
// unique within a project, so the project label is part of it.
func buildPodSelector(project, name string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{builds.LabelBuild: name, controllers.LabelProject: project})
}

// buildLogs streams the build pod's log as Server-Sent Events, exactly like
// the App and Task logs (api_logs.go): the same events, query and limits. It
// follows while the build runs and waits for the pod of a queued build. A
// finished build whose pod is gone is served from log history (VictoriaLogs,
// see logHistory: the start event says "source": "history"); when that has
// nothing either, it gets a start event and an end event with reason "gone".
func (b *buildsAPI) buildLogs(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("build")
	lq, ok := b.logs.parseLogQuery(w, r)
	if !ok {
		return
	}
	pr := r.Context().Value(ctxKey{}).(*principal)
	target := appTarget(project, name)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	// The user's own get decides; everything after it uses the console's identity.
	build, err := b.getBuild(ctx, pr, project, name)
	if err != nil {
		b.kubeError(w, r, pr, "build.logs", target, buildNotFound(project, name), err)
		return
	}
	own, err := b.ownPods(ctx)
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	sel := buildPodSelector(project, name)
	podsOf := func(ctx context.Context) ([]corev1.Pod, error) {
		pods, err := own.listPods(ctx, builds.Namespace, sel)
		if lq.pod != "" {
			pods = slices.DeleteFunc(pods, func(pod corev1.Pod) bool { return pod.Name != lq.pod })
		}
		return pods, err
	}
	pods, err := podsOf(ctx)
	if err != nil {
		if apierrors.IsForbidden(err) {
			err = fmt.Errorf("the console's service account may not read pods in %s: %w", builds.Namespace, err)
		}
		b.internalError(w, r, err)
		return
	}
	if lq.pod != "" && len(pods) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Pod %q is not part of build %q.", lq.pod, name))
		return
	}
	if lq.container != "" && len(pods) > 0 && !slices.ContainsFunc(pods[0].Spec.Containers, func(c corev1.Container) bool { return c.Name == lq.container }) {
		invalid(w, "container", fmt.Sprintf("There is no container %q in this build.", lq.container))
		return
	}
	finished := buildFinished(build.Status.Phase)
	if finished {
		lq.follow = false // nothing more will come
	}
	if finished && len(pods) == 0 {
		// The pod is gone: its log may still be in VictoriaLogs, read with
		// the console's identity like the pod, confined to this Build's pods.
		start, end := historyRange(build.CreationTimestamp, build.Status.CompletionTime, b.now())
		scope := logs.Scope{Namespaces: []string{builds.Namespace}, Fields: map[string]string{logs.FieldBuild: name, logs.FieldProject: project}}
		if res := b.historyLog(r.Context(), scope, start, end, lq.tail, lq.container); res != nil {
			sendHistory(w, b.logs.writeTimeout, res, lq.container)
			return
		}
	}
	release := b.streams.acquire(pr.user.Email)
	if release == nil {
		writeError(w, http.StatusTooManyRequests, "Too many log streams are open. Close another log view and try again.")
		return
	}
	defer release()
	b.running.Add(1)
	defer b.running.Add(-1)

	s := &logStream{
		b: own, ns: builds.Namespace, pods: podsOf, q: lq, lim: b.logs,
		alive: func(ctx context.Context) bool { return b.stillValid(ctx, pr) },
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s.out = &sseWriter{w: w, rc: http.NewResponseController(w), timeout: b.logs.writeTimeout}
	if finished && len(pods) == 0 {
		if s.out.event("start", map[string]any{"pods": []string{}, "container": "", "follow": false, "previous": false}) == nil {
			_ = s.end("gone", "This build's log is no longer available: its pod has been removed.")
		}
		return
	}
	s.run(r.Context(), pods)
}

// ---- validation of Git sources ---------------------------------------------------

var (
	// https://host[:port]/owner/repo[.git], also deeper (GitLab subgroups).
	httpsRepoRE = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%+-]+)+/?$`)
	// ssh://[user@]host[:port]/owner/repo[.git]
	sshRepoRE = regexp.MustCompile(`^ssh://([A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%+-]+)+/?$`)
	// scp-like: git@host:owner/repo[.git]
	scpRepoRE   = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._~%+-]+(/[A-Za-z0-9._~%+-]+)*/?$`)
	branchRE    = regexp.MustCompile(`^[A-Za-z0-9._/+@-]+$`)
	repoPathRE  = regexp.MustCompile(`^[^\x00-\x1f\x7f\\]*$`)
	dotSegments = regexp.MustCompile(`(^|/)\.\.(/|$)`)
)

// repositoryProblem says what is wrong with a repository URL, or "".
func repositoryProblem(repo string) string {
	switch {
	case repo == "":
		return "Enter the repository's URL, like https://github.com/acme/api.git or git@github.com:acme/api.git."
	case strings.HasPrefix(repo, "http://"):
		return "Use https:// or SSH (git@host:owner/repo.git); Kwerft does not clone over plain HTTP."
	case strings.HasPrefix(repo, "https://") && strings.Contains(strings.SplitN(repo[len("https://"):], "/", 2)[0], "@"):
		return "Leave credentials out of the URL. Add a Git connection in Settings instead."
	case dotSegments.MatchString(repo):
		return "The URL must not contain \"..\"."
	case httpsRepoRE.MatchString(repo), sshRepoRE.MatchString(repo), scpRepoRE.MatchString(repo):
		return ""
	}
	return "Use https://host/owner/repo.git or git@host:owner/repo.git."
}

// branchProblem follows git check-ref-format for the names people use.
func branchProblem(branch string) string {
	if branch == "" {
		return ""
	}
	if len(branch) > 255 || !branchRE.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") ||
		strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") || strings.HasPrefix(branch, ".") ||
		strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, ".lock") || strings.Contains(branch, "@{") {
		return fmt.Sprintf("%q is not a valid branch name.", branch)
	}
	return ""
}

// repoPathProblem checks a path inside the repository (build context or
// Dockerfile): it must stay inside the checkout.
func repoPathProblem(p string) string {
	if len(p) > 255 || !repoPathRE.MatchString(p) {
		return "Use a path inside the repository, like / or services/api."
	}
	if dotSegments.MatchString(p) {
		return "The path must stay inside the repository: no \"..\"."
	}
	return ""
}

// validateGitSource is part of validateSpec for Apps built from Git. The CRD
// schema checks the enum and that a repository is set; this explains the
// rest in words, next to the field.
func validateGitSource(w http.ResponseWriter, g *kwerftv1.GitSource) bool {
	const f = "spec.source.git."
	if msg := repositoryProblem(g.Repository); msg != "" {
		invalid(w, f+"repository", msg)
		return false
	}
	if msg := branchProblem(g.Branch); msg != "" {
		invalid(w, f+"branch", msg)
		return false
	}
	if msg := repoPathProblem(g.Path); msg != "" {
		invalid(w, f+"path", msg)
		return false
	}
	if g.Builder != "" && g.Builder != "dockerfile" && g.Builder != "railpack" {
		invalid(w, f+"builder", "Build with dockerfile or railpack.")
		return false
	}
	if g.Dockerfile != "" {
		if msg := repoPathProblem(g.Dockerfile); msg != "" {
			invalid(w, f+"dockerfile", msg)
			return false
		}
		if strings.HasSuffix(g.Dockerfile, "/") {
			invalid(w, f+"dockerfile", "Name the Dockerfile itself, like Dockerfile or docker/Dockerfile.prod.")
			return false
		}
	}
	if g.Connection != "" && len(validation.IsDNS1123Subdomain(g.Connection)) > 0 {
		invalid(w, f+"connection", fmt.Sprintf("%q is not a valid Git connection name.", g.Connection))
		return false
	}
	return true
}

// checkGitConnection makes sure an App's Git connection exists and may be
// used in the project, so the user hears it now rather than from a failed
// build. Anything but a clear answer (no access, the CRD missing) lets the
// request through: the Build reconciler reports problems as well. Git
// connections live in the management cluster (docs/phase5.md), so they are
// read there as the user, whatever cluster the project is in.
func (a *api) checkGitConnection(ctx context.Context, w http.ResponseWriter, p *principal, project string, g *kwerftv1.GitSource) bool {
	if g == nil || g.Connection == "" {
		return true
	}
	c, err := a.managementClient(p)
	if err != nil {
		return true
	}
	var conn kwerftv1.GitConnection
	err = c.Get(ctx, types.NamespacedName{Name: g.Connection}, &conn)
	switch {
	case apierrors.IsNotFound(err):
		invalid(w, "spec.source.git.connection", fmt.Sprintf("There is no Git connection %q. Owners and admins add them in Settings.", g.Connection))
		return false
	case err != nil:
		return true
	}
	if len(conn.Spec.Projects) > 0 && !slices.Contains(conn.Spec.Projects, project) {
		invalid(w, "spec.source.git.connection", fmt.Sprintf("The Git connection %q is not available in project %q.", g.Connection, project))
		return false
	}
	return true
}
