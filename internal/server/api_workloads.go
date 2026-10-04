package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Workload API: projects and apps.
//
// Who talks to Kubernetes:
//   - Every write, and every read of a single object (which carries env
//     values, commands and the like), goes to the API server as the signed-in
//     user through impersonation (internal/kube). Kubernetes RBAC — the
//     kwerft:<role> ClusterRoles in the chart — decides; the console only
//     translates its answer.
//   - Lists of Projects and Apps (polled every few seconds by the console)
//     may be served from the controller manager's informer cache. That is
//     safe only because every console role may list every kwerft.dev
//     resource cluster-wide, and the summaries leave out env and commands.
//     When roles become per-project (Phase 4), these lists must filter by the
//     user's projects or go through impersonation as well.
//   - Nothing here reads Secrets, Pods or logs.

const kubeTimeout = 20 * time.Second

func (a *api) registerWorkloads(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }

	mux.HandleFunc("GET /api/v1/projects", read(a.projectList))
	mux.HandleFunc("POST /api/v1/projects", write(a.projectCreate))
	mux.HandleFunc("DELETE /api/v1/projects/{project}", write(a.projectDelete))

	mux.HandleFunc("GET /api/v1/apps", read(a.appList))
	mux.HandleFunc("POST /api/v1/projects/{project}/apps", write(a.appCreate))
	mux.HandleFunc("GET /api/v1/projects/{project}/apps/{app}", read(a.appGet))
	mux.HandleFunc("PUT /api/v1/projects/{project}/apps/{app}", write(a.appUpdate))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/apps/{app}", write(a.appDelete))
	mux.HandleFunc("POST /api/v1/projects/{project}/apps/{app}/rollback", write(a.appRollback))
	mux.HandleFunc("POST /api/v1/projects/{project}/apps/{app}/restart", write(a.appRestart))
	mux.HandleFunc("PATCH /api/v1/projects/{project}/apps/{app}/scale", write(a.appScale))
}

func (a *api) requireKube(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.Kube == nil {
			writeError(w, http.StatusServiceUnavailable, "This console is not connected to a Kubernetes cluster.")
			return
		}
		next(w, r)
	}
}

// userClient returns a client that acts as the signed-in user, and a context
// bounded by kubeTimeout.
func (a *api) userClient(r *http.Request) (client.Client, *principal, context.Context, context.CancelFunc, error) {
	p := r.Context().Value(ctxKey{}).(*principal)
	c, err := a.cfg.Kube.For(p.user.Email, p.user.Role)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	return c, p, ctx, cancel, err
}

// list reads from the informer cache when there is one (see the rule above)
// and falls back to the user's own client before the cache has started.
func (a *api) list(ctx context.Context, c client.Client, list client.ObjectList, opts ...client.ListOption) error {
	if a.cfg.KubeCache != nil {
		err := a.cfg.KubeCache.List(ctx, list, opts...)
		var notStarted *cache.ErrCacheNotStarted
		if !errors.As(err, &notStarted) {
			return err
		}
	}
	return c.List(ctx, list, opts...)
}

// kubeError translates an API server answer for the console. notFound is the
// message for a 404 (the caller knows what was missing).
func (a *api) kubeError(w http.ResponseWriter, r *http.Request, p *principal, action, target, notFound string, err error) {
	var status apierrors.APIStatus
	switch {
	case apierrors.IsForbidden(err):
		a.audit(r, p.user.Email, action+".denied", target, "forbidden by Kubernetes RBAC")
		writeError(w, http.StatusForbidden, "Your role does not allow this.")
	case apierrors.IsNotFound(err):
		if errors.As(err, &status) && status.Status().Details != nil && status.Status().Details.Kind == "namespaces" {
			notFound = fmt.Sprintf("Project %q does not exist, or is still being set up.", status.Status().Details.Name)
		}
		writeError(w, http.StatusNotFound, notFound)
	case apierrors.IsAlreadyExists(err):
		writeError(w, http.StatusConflict, strings.TrimSuffix(notFound, " not found.")+" already exists.")
	case apierrors.IsConflict(err):
		writeError(w, http.StatusConflict, "Someone else changed this in the meantime. Reload and try again.")
	case apierrors.IsInvalid(err):
		field, msg := "", err.Error()
		if errors.As(err, &status) && status.Status().Details != nil && len(status.Status().Details.Causes) > 0 {
			cause := status.Status().Details.Causes[0]
			field, msg = cause.Field, humanize(cause.Field, cause.Message)
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "field": field})
	case apierrors.IsBadRequest(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err),
		apierrors.IsTooManyRequests(err), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "The Kubernetes API is not responding. Try again in a moment.")
	default:
		a.internalError(w, r, err)
	}
}

// humanize turns an API server validation message such as "Invalid value: -1:
// spec.replicas in body should be greater than or equal to 0" into "Should be
// greater than or equal to 0." — the console shows it next to the field.
func humanize(field, msg string) string {
	if msg == "Required value" {
		return "This field is required."
	}
	msg = strings.TrimPrefix(msg, "Invalid value: ")
	if _, after, ok := strings.Cut(msg, field+" in body "); ok {
		msg = after
	}
	if msg == "" {
		return "This value is not valid."
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

func invalid(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "field": field})
}

// decodeStrict is decode for Kubernetes-shaped bodies: unknown fields are an
// error, so a typo in a spec does not silently do nothing.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "The request body is not valid: "+err.Error())
		return false
	}
	return true
}

// ---- projects ----------------------------------------------------------------

type projectJSON struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName,omitempty"`
	Phase       string    `json:"phase"` // ready | pending | failed
	Reason      string    `json:"reason,omitempty"`
	Message     string    `json:"message,omitempty"`
	Apps        int       `json:"apps"`
	Created     time.Time `json:"created"`
}

func projectSummary(p *kwerftv1.Project, apps int) projectJSON {
	out := projectJSON{Name: p.Name, DisplayName: p.Spec.DisplayName, Apps: apps, Created: p.CreationTimestamp.UTC(), Phase: "pending"}
	if c := meta.FindStatusCondition(p.Status.Conditions, controllers.ConditionReady); c != nil {
		out.Reason, out.Message = c.Reason, c.Message
		switch {
		case c.ObservedGeneration < p.Generation:
		case c.Status == metav1.ConditionTrue:
			out.Phase = "ready"
		default:
			out.Phase = "failed"
		}
	}
	return out
}

func (a *api) projectList(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var projects kwerftv1.ProjectList
	if err := a.list(ctx, c, &projects); err != nil {
		a.kubeError(w, r, p, "project.list", "", "No projects found.", err)
		return
	}
	var apps kwerftv1.AppList
	if err := a.list(ctx, c, &apps); err != nil {
		a.kubeError(w, r, p, "project.list", "", "No apps found.", err)
		return
	}
	count := map[string]int{}
	for _, app := range apps.Items {
		count[app.Namespace]++
	}
	out := make([]projectJSON, 0, len(projects.Items))
	for i := range projects.Items {
		out = append(out, projectSummary(&projects.Items[i], count[projects.Items[i].Name]))
	}
	slices.SortFunc(out, func(x, y projectJSON) int { return strings.Compare(x.Name, y.Name) })
	writeJSON(w, http.StatusOK, out)
}

// Namespaces a project must never take over.
var reservedProjects = []string{"default", "kwerft-system", "cert-manager", "traefik", "monitoring"}

func (a *api) projectCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Name, req.DisplayName = strings.TrimSpace(req.Name), strings.TrimSpace(req.DisplayName)
	if errs := validation.IsDNS1123Label(req.Name); len(errs) > 0 {
		invalid(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 63).")
		return
	}
	if slices.Contains(reservedProjects, req.Name) || strings.HasPrefix(req.Name, "kube-") {
		invalid(w, "name", fmt.Sprintf("%q is reserved for the platform. Pick another name.", req.Name))
		return
	}
	if len(req.DisplayName) > 100 {
		invalid(w, "displayName", "Keep the display name under 100 characters.")
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	proj := &kwerftv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name},
		Spec:       kwerftv1.ProjectSpec{DisplayName: req.DisplayName},
	}
	if err := c.Create(ctx, proj); err != nil {
		a.kubeError(w, r, p, "project.create", req.Name, fmt.Sprintf("Project %q not found.", req.Name), err)
		return
	}
	a.audit(r, p.user.Email, "project.create", req.Name, "")
	writeJSON(w, http.StatusCreated, projectSummary(proj, 0))
}

func (a *api) projectDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := c.Delete(ctx, &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		a.kubeError(w, r, p, "project.delete", name, fmt.Sprintf("Project %q not found.", name), err)
		return
	}
	a.audit(r, p.user.Email, "project.delete", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// ---- apps --------------------------------------------------------------------

type appSourceJSON struct {
	Type       string `json:"type"` // image | git
	Image      string `json:"image,omitempty"`
	Repository string `json:"repository,omitempty"`
	Branch     string `json:"branch,omitempty"`
}

type appSummaryJSON struct {
	Name     string        `json:"name"`
	Project  string        `json:"project"`
	Source   appSourceJSON `json:"source"`
	Image    string        `json:"image"` // what runs (or will run)
	Ready    int32         `json:"readyReplicas"`
	Desired  int32         `json:"replicas"`
	Stateful bool          `json:"stateful"`
	Phase    string        `json:"phase"` // running | deploying | stopped | pending | failed
	Reason   string        `json:"reason,omitempty"`
	Message  string        `json:"message,omitempty"`
	URLs     []string      `json:"urls"`
	Revision int64         `json:"revision"`
	Updated  time.Time     `json:"updated"`
}

// appPhase condenses the Ready condition into the five states the console
// shows as status pills.
func appPhase(app *kwerftv1.App) (phase, reason, message string) {
	c := meta.FindStatusCondition(app.Status.Conditions, controllers.ConditionReady)
	switch {
	case c == nil:
		return "pending", "Pending", "Waiting for the controller to pick up this app."
	case c.ObservedGeneration < app.Generation:
		return "deploying", "Updating", "Rolling out the latest change."
	case c.Status == metav1.ConditionTrue && c.Reason == "ScaledToZero":
		return "stopped", c.Reason, c.Message
	case c.Status == metav1.ConditionTrue:
		return "running", c.Reason, c.Message
	case c.Reason == "Progressing":
		return "deploying", c.Reason, c.Message
	case c.Reason == "AwaitingBuild":
		return "pending", c.Reason, c.Message
	default:
		return "failed", c.Reason, c.Message
	}
}

func appSummary(app *kwerftv1.App) appSummaryJSON {
	out := appSummaryJSON{
		Name: app.Name, Project: app.Namespace, Image: app.Status.Image, Ready: app.Status.ReadyReplicas,
		Desired: 1, Stateful: len(app.Spec.Volumes) > 0, URLs: app.Status.URLs, Revision: app.Status.Revision,
		Updated: app.CreationTimestamp.UTC(),
	}
	if app.Spec.Replicas != nil {
		out.Desired = *app.Spec.Replicas
	}
	if out.URLs == nil {
		out.URLs = []string{}
	}
	switch src := app.Spec.Source; {
	case src.Image != nil:
		out.Source = appSourceJSON{Type: "image", Image: src.Image.Ref}
		if out.Image == "" {
			out.Image = src.Image.Ref
		}
	case src.Git != nil:
		out.Source = appSourceJSON{Type: "git", Repository: src.Git.Repository, Branch: src.Git.Branch}
	}
	out.Phase, out.Reason, out.Message = appPhase(app)
	if h := app.Status.History; len(h) > 0 && h[0].Time.After(out.Updated) {
		out.Updated = h[0].Time.UTC()
	}
	if c := meta.FindStatusCondition(app.Status.Conditions, controllers.ConditionReady); c != nil && c.LastTransitionTime.After(out.Updated) {
		out.Updated = c.LastTransitionTime.UTC()
	}
	return out
}

// appJSON is the App as Kubernetes stores it, minus server-side bookkeeping.
func appJSON(app *kwerftv1.App) *kwerftv1.App {
	out := app.DeepCopy()
	out.APIVersion, out.Kind = kwerftv1.GroupVersion.String(), "App"
	out.ManagedFields = nil
	return out
}

func appNotFound(project, name string) string {
	return fmt.Sprintf("App %q in project %q not found.", name, project)
}

func appTarget(project, name string) string { return project + "/" + name }

func (a *api) appList(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var opts []client.ListOption
	if project := r.URL.Query().Get("project"); project != "" {
		opts = append(opts, client.InNamespace(project))
	}
	var apps kwerftv1.AppList
	if err := a.list(ctx, c, &apps, opts...); err != nil {
		a.kubeError(w, r, p, "app.list", "", "No apps found.", err)
		return
	}
	out := make([]appSummaryJSON, 0, len(apps.Items))
	for i := range apps.Items {
		out = append(out, appSummary(&apps.Items[i]))
	}
	slices.SortFunc(out, func(x, y appSummaryJSON) int {
		if n := strings.Compare(x.Project, y.Project); n != 0 {
			return n
		}
		return strings.Compare(x.Name, y.Name)
	})
	writeJSON(w, http.StatusOK, out)
}

var hostnameRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`)

// validateSpec catches mistakes the CRD schema lets through but the
// reconciler would trip over later, so the user hears about them now.
func validateSpec(w http.ResponseWriter, spec *kwerftv1.AppSpec) bool {
	for i, e := range spec.Env {
		if errs := validation.IsEnvVarName(e.Name); len(errs) > 0 {
			invalid(w, fmt.Sprintf("spec.env[%d].name", i), fmt.Sprintf("%q is not a valid variable name. Use letters, digits, _, - and ., not starting with a digit.", e.Name))
			return false
		}
	}
	seen := map[int32]bool{}
	for i, port := range spec.Ports {
		if seen[port.Container] {
			invalid(w, fmt.Sprintf("spec.ports[%d].container", i), fmt.Sprintf("Port %d is listed twice.", port.Container))
			return false
		}
		seen[port.Container] = true
		if port.Public != "" && (len(port.Public) > 253 || !hostnameRE.MatchString(port.Public)) {
			invalid(w, fmt.Sprintf("spec.ports[%d].public", i), fmt.Sprintf("%q is not a valid hostname. Use lowercase, like app.example.com.", port.Public))
			return false
		}
	}
	return true
}

func (a *api) appCreate(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req struct {
		Name string           `json:"name"`
		Spec kwerftv1.AppSpec `json:"spec"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if errs := validation.IsDNS1035Label(req.Name); len(errs) > 0 {
		invalid(w, "name", "Use lowercase letters, digits and dashes, starting with a letter (at most 63).")
		return
	}
	if !validateSpec(w, &req.Spec) {
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: project}, Spec: req.Spec}
	target := appTarget(project, req.Name)
	if err := c.Create(ctx, app); err != nil {
		a.kubeError(w, r, p, "app.create", target, appNotFound(project, req.Name), err)
		return
	}
	a.audit(r, p.user.Email, "app.create", target, describeSource(app))
	writeJSON(w, http.StatusCreated, appJSON(app))
}

func describeSource(app *kwerftv1.App) string {
	switch src := app.Spec.Source; {
	case src.Image != nil:
		return "image " + src.Image.Ref
	case src.Git != nil:
		return "git " + src.Git.Repository
	}
	return ""
}

func (a *api) appGet(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var app kwerftv1.App
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &app); err != nil {
		a.kubeError(w, r, p, "app.get", appTarget(project, name), appNotFound(project, name), err)
		return
	}
	writeJSON(w, http.StatusOK, appJSON(&app))
}

// appUpdate replaces the spec. With resourceVersion (from the object the
// user edited) a concurrent change is a 409 instead of being overwritten.
func (a *api) appUpdate(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	var req struct {
		Spec            kwerftv1.AppSpec `json:"spec"`
		ResourceVersion string           `json:"resourceVersion"`
	}
	if !decodeStrict(w, r, &req) || !validateSpec(w, &req.Spec) {
		return
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
		a.kubeError(w, r, p, "app.update", target, appNotFound(project, name), err)
		return
	}
	if req.ResourceVersion != "" {
		app.ResourceVersion = req.ResourceVersion
	}
	app.Spec = req.Spec
	if err := c.Update(ctx, &app); err != nil {
		a.kubeError(w, r, p, "app.update", target, appNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "app.update", target, fmt.Sprintf("generation %d", app.Generation))
	writeJSON(w, http.StatusOK, appJSON(&app))
}

func (a *api) appDelete(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	if err := c.Delete(ctx, &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}); err != nil {
		a.kubeError(w, r, p, "app.delete", target, appNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "app.delete", target, "")
	w.WriteHeader(http.StatusNoContent)
}

// appRollback runs the image of an earlier revision again, as a new
// revision. Settings (env, replicas, ports, ...) stay as they are now: a
// revision records the image it ran, not the whole spec. Image apps get the
// old reference back; Git apps pin the old build's image until the pin is
// cleared.
func (a *api) appRollback(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	var req struct {
		Revision int64 `json:"revision"`
	}
	if !decode(w, r, &req) {
		return
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
		a.kubeError(w, r, p, "app.rollback", target, appNotFound(project, name), err)
		return
	}
	idx := slices.IndexFunc(app.Status.History, func(rev kwerftv1.AppRevision) bool { return rev.Number == req.Revision })
	if idx < 0 {
		invalid(w, "revision", fmt.Sprintf("Revision %d is not in this app's history.", req.Revision))
		return
	}
	image := app.Status.History[idx].Image
	switch src := &app.Spec.Source; {
	case src.Image != nil:
		if src.Image.Ref == image {
			writeError(w, http.StatusConflict, fmt.Sprintf("Revision %d runs the current image already.", req.Revision))
			return
		}
		src.Image.Ref = image
	case src.Git != nil:
		current := src.Git.PinnedImage
		if current == "" {
			current = app.Status.Image
		}
		if current == image {
			writeError(w, http.StatusConflict, fmt.Sprintf("Revision %d runs the current image already.", req.Revision))
			return
		}
		src.Git.PinnedImage = image
	default:
		invalid(w, "spec.source", "This app has no source to roll back.")
		return
	}
	if err := c.Update(ctx, &app); err != nil {
		a.kubeError(w, r, p, "app.rollback", target, appNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "app.rollback", target, fmt.Sprintf("to revision %d (%s)", req.Revision, image))
	writeJSON(w, http.StatusOK, appJSON(&app))
}

// appRestart replaces every replica by changing an annotation the reconciler
// copies onto the pod template (see kwerftv1.AnnotationRestartedAt).
func (a *api) appRestart(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		kwerftv1.AnnotationRestartedAt: a.now().UTC().Format(time.RFC3339Nano),
	}}})
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}
	if err := c.Patch(ctx, app, client.RawPatch(types.MergePatchType, patch)); err != nil {
		a.kubeError(w, r, p, "app.restart", target, appNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "app.restart", target, "")
	writeJSON(w, http.StatusOK, appJSON(app))
}

func (a *api) appScale(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	var req struct {
		Replicas *int32 `json:"replicas"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Replicas == nil || *req.Replicas < 0 {
		invalid(w, "replicas", "Enter a number of replicas, 0 or more.")
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"replicas": *req.Replicas}})
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}
	if err := c.Patch(ctx, app, client.RawPatch(types.MergePatchType, patch)); err != nil {
		a.kubeError(w, r, p, "app.scale", target, appNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "app.scale", target, fmt.Sprintf("replicas %d", *req.Replicas))
	writeJSON(w, http.StatusOK, appJSON(app))
}
