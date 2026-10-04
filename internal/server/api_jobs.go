package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Jobs API: shared Volumes, Tasks (one-off runs), Schedules, and the
// read-only list of Domains.
//
// The access rule of api_workloads.go holds here too: every write and every
// read of a single object goes through impersonation, so Kubernetes RBAC
// decides; the polled lists (volumes, tasks, schedules, domains) may come from
// the informer cache, confined to the user's project scope (scope.go), and
// leave out env values and commands (a Task summary names its env overrides,
// not their values).

func (a *api) registerJobs(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }

	mux.HandleFunc("GET /api/v1/volumes", read(a.withClusterParam(a.volumeList)))
	mux.HandleFunc("POST /api/v1/projects/{project}/volumes", write(a.volumeCreate))
	mux.HandleFunc("PATCH /api/v1/projects/{project}/volumes/{volume}", write(a.volumeResize))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/volumes/{volume}", write(a.volumeDelete))

	mux.HandleFunc("GET /api/v1/tasks", read(a.withClusterParam(a.taskList)))
	mux.HandleFunc("POST /api/v1/projects/{project}/tasks", write(a.taskCreate))
	mux.HandleFunc("GET /api/v1/projects/{project}/tasks/{task}", read(a.taskGet))
	mux.HandleFunc("POST /api/v1/projects/{project}/tasks/{task}/cancel", write(a.taskCancel))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/tasks/{task}", write(a.taskDelete))

	mux.HandleFunc("GET /api/v1/schedules", read(a.withClusterParam(a.scheduleList)))
	mux.HandleFunc("GET /api/v1/schedules/preview", a.requireUser(a.schedulePreview))
	mux.HandleFunc("POST /api/v1/projects/{project}/schedules", write(a.scheduleCreate))
	mux.HandleFunc("GET /api/v1/projects/{project}/schedules/{schedule}", read(a.scheduleGet))
	mux.HandleFunc("PUT /api/v1/projects/{project}/schedules/{schedule}", write(a.scheduleUpdate))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/schedules/{schedule}", write(a.scheduleDelete))
	mux.HandleFunc("POST /api/v1/projects/{project}/schedules/{schedule}/suspend", write(a.scheduleSuspend(true)))
	mux.HandleFunc("POST /api/v1/projects/{project}/schedules/{schedule}/resume", write(a.scheduleSuspend(false)))
	mux.HandleFunc("POST /api/v1/projects/{project}/schedules/{schedule}/run", write(a.scheduleRun))

	mux.HandleFunc("GET /api/v1/domains", read(a.withClusterParam(a.domainList)))
}

// decodeOptional is decodeStrict for bodies that may be left out entirely.
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "The request body is not valid: "+err.Error())
		return false
	}
	return true
}

func inProject(r *http.Request) []client.ListOption {
	if project := r.URL.Query().Get("project"); project != "" {
		return []client.ListOption{client.InNamespace(project)}
	}
	return nil
}

func byProjectAndName(xp, xn, yp, yn string) int {
	if n := strings.Compare(xp, yp); n != 0 {
		return n
	}
	return strings.Compare(xn, yn)
}

func timePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// stripObject drops server-side bookkeeping and fills in the type, so a
// single object reads like the YAML a user would apply.
func stripObject(obj client.Object, kind string) {
	obj.SetManagedFields(nil)
	obj.GetObjectKind().SetGroupVersionKind(kwerftv1.GroupVersion.WithKind(kind))
}

// ---- volumes -----------------------------------------------------------------

type volumeJSON struct {
	Name     string    `json:"name"`
	Project  string    `json:"project"`
	Cluster  string    `json:"cluster,omitempty"`
	Size     string    `json:"size"`
	Class    string    `json:"class"`
	Capacity string    `json:"capacity,omitempty"`
	Phase    string    `json:"phase"` // bound | pending | lost | failed | deleting
	Reason   string    `json:"reason,omitempty"`
	Message  string    `json:"message,omitempty"`
	UsedBy   []string  `json:"usedBy"`
	Created  time.Time `json:"created"`
}

func volumeSummary(v *kwerftv1.Volume) volumeJSON {
	out := volumeJSON{
		Name: v.Name, Project: v.Namespace, Size: v.Spec.Size.String(), Class: v.Spec.Class,
		UsedBy: v.Status.UsedBy, Created: v.CreationTimestamp.UTC(), Phase: "pending",
	}
	if out.Class == "" {
		out.Class = "local-nvme"
	}
	if out.UsedBy == nil {
		out.UsedBy = []string{}
	}
	if c := v.Status.Capacity; c != nil {
		out.Capacity = c.String()
	}
	c := meta.FindStatusCondition(v.Status.Conditions, controllers.ConditionReady)
	if c != nil {
		out.Reason, out.Message = c.Reason, c.Message
	}
	switch {
	case !v.DeletionTimestamp.IsZero():
		out.Phase = "deleting"
		if c == nil || c.Reason != "InUse" {
			out.Reason, out.Message = "Deleting", "Being deleted"
		}
	case v.Status.Phase == corev1.ClaimBound:
		out.Phase = "bound"
	case v.Status.Phase == corev1.ClaimLost:
		out.Phase = "lost"
	case c != nil && c.Status == metav1.ConditionFalse && c.ObservedGeneration >= v.Generation && c.Reason != "Pending":
		out.Phase = "failed"
	}
	return out
}

func volumeNotFound(project, name string) string {
	return fmt.Sprintf("Volume %q in project %q not found.", name, project)
}

func (a *api) volumeList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	out := []volumeJSON{}
	if !a.visitClusters(w, r, ctx, p, "volume.list", "No volumes found.", func(v *clusterVisit) error {
		var list kwerftv1.VolumeList
		if err := a.scopedList(v.ctx, v.c, v.scope, &list, inProject(r)...); err != nil {
			return err
		}
		for i := range list.Items {
			s := volumeSummary(&list.Items[i])
			s.Cluster = v.conn.name
			out = append(out, s)
		}
		return nil
	}) {
		return
	}
	slices.SortFunc(out, func(x, y volumeJSON) int { return byProjectAndName(x.Project, x.Name, y.Project, y.Name) })
	writeJSON(w, http.StatusOK, out)
}

func (a *api) volumeCreate(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req struct {
		Name  string `json:"name"`
		Size  string `json:"size"`
		Class string `json:"class"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if errs := validation.IsDNS1123Label(req.Name); len(errs) > 0 {
		invalid(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 63).")
		return
	}
	size, ok := parseSize(w, "size", req.Size)
	if !ok {
		return
	}
	if req.Class != "" && req.Class != "local-nvme" && req.Class != "hcloud-volume" {
		invalid(w, "class", "Choose local-nvme or hcloud-volume.")
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	vol := &kwerftv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: project},
		Spec:       kwerftv1.VolumeSpec{Size: size, Class: req.Class},
	}
	target := appTarget(project, req.Name)
	if err := c.Create(ctx, vol); err != nil {
		a.kubeError(w, r, p, "volume.create", target, volumeNotFound(project, req.Name), err)
		return
	}
	a.audit(r, p.user.Email, "volume.create", target, fmt.Sprintf("%s %s", size.String(), vol.Spec.Class))
	writeJSON(w, http.StatusCreated, volumeSummary(vol))
}

// parseSize reads a disk size such as 10Gi; a bare number means GiB.
func parseSize(w http.ResponseWriter, field, s string) (resource.Quantity, bool) {
	s = strings.TrimSpace(s)
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		s += "Gi"
	}
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() <= 0 {
		invalid(w, field, "Enter a size such as 10Gi or 500Mi.")
		return q, false
	}
	return q, true
}

// volumeResize grows a Volume. Shrinking is refused here with the field
// named (the CRD refuses it too), and so is growing a local-nvme disk, which
// its storage class cannot do.
func (a *api) volumeResize(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("volume")
	var req struct {
		Size string `json:"size"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	size, ok := parseSize(w, "size", req.Size)
	if !ok {
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	var vol kwerftv1.Volume
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &vol); err != nil {
		a.kubeError(w, r, p, "volume.resize", target, volumeNotFound(project, name), err)
		return
	}
	switch cmp := size.Cmp(vol.Spec.Size); {
	case cmp < 0:
		invalid(w, "size", fmt.Sprintf("A volume can only grow. It is %s now.", vol.Spec.Size.String()))
		return
	case cmp == 0:
		writeJSON(w, http.StatusOK, volumeSummary(&vol))
		return
	case vol.Spec.Class == "" || vol.Spec.Class == "local-nvme":
		invalid(w, "size", "A local-nvme volume cannot be resized. Create a larger volume and copy the data with a task.")
		return
	}
	// A patch of the size alone: the reconciler adds its finalizer and
	// status right after creation, which an Update would trip over. The CRD
	// still refuses a shrink if the size changed since the read above.
	old := vol.Spec.Size.String()
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"size": size.String()}})
	if err := c.Patch(ctx, &vol, client.RawPatch(types.MergePatchType, patch)); err != nil {
		a.kubeError(w, r, p, "volume.resize", target, volumeNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "volume.resize", target, fmt.Sprintf("%s → %s", old, size.String()))
	writeJSON(w, http.StatusOK, volumeSummary(&vol))
}

// volumeDelete deletes a Volume. While Apps, Schedules or unfinished Tasks
// still mount it, deletion waits (the volume-protection finalizer); the
// answer is then 202 and names them, instead of a silent wait.
func (a *api) volumeDelete(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("volume")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	if err := c.Delete(ctx, &kwerftv1.Volume{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}); err != nil {
		a.kubeError(w, r, p, "volume.delete", target, volumeNotFound(project, name), err)
		return
	}
	users, err := controllers.VolumeUsers(ctx, c, project, name)
	if err != nil {
		a.cfg.Logger.Warn("list volume users", "volume", target, "err", err)
	}
	if len(users) == 0 {
		a.audit(r, p.user.Email, "volume.delete", target, "")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.audit(r, p.user.Email, "volume.delete", target, "waits for "+strings.Join(users, ", "))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"usedBy":  users,
		"reason":  "InUse",
		"message": fmt.Sprintf("Volume %s is deleted once %s no longer mount it.", name, strings.Join(users, ", ")),
	})
}

// ---- tasks -------------------------------------------------------------------

type taskJSON struct {
	Name        string     `json:"name"`
	Project     string     `json:"project"`
	Cluster     string     `json:"cluster,omitempty"`
	Phase       string     `json:"phase"` // pending | running | succeeded | failed
	Reason      string     `json:"reason,omitempty"`
	Message     string     `json:"message,omitempty"`
	FromApp     string     `json:"fromApp,omitempty"`
	Schedule    string     `json:"schedule,omitempty"`
	Image       string     `json:"image,omitempty"`
	StartedBy   string     `json:"startedBy,omitempty"` // a console user; empty for a scheduled run
	ScheduledAt *time.Time `json:"scheduledAt,omitempty"`
	Overrides   []string   `json:"overrides"` // names of envOverrides; values only in the Task itself
	Restart     []string   `json:"restart"`
	Created     time.Time  `json:"created"`
	Started     *time.Time `json:"started,omitempty"`
	Finished    *time.Time `json:"finished,omitempty"`
	ExitCode    *int32     `json:"exitCode,omitempty"`
}

func taskPhase(t *kwerftv1.Task) string {
	if t.Status.Phase == "" {
		return "pending"
	}
	return strings.ToLower(string(t.Status.Phase))
}

func taskSummary(t *kwerftv1.Task) taskJSON {
	out := taskJSON{
		Name: t.Name, Project: t.Namespace, Phase: taskPhase(t), FromApp: t.Spec.FromApp,
		Schedule: t.Labels[controllers.LabelSchedule], Image: t.Status.Image,
		StartedBy: t.Annotations[kwerftv1.AnnotationStartedBy], Overrides: []string{}, Restart: []string{},
		Created: t.CreationTimestamp.UTC(), Started: timePtr(t.Status.StartTime), Finished: timePtr(t.Status.CompletionTime),
		ExitCode: t.Status.ExitCode,
	}
	if out.Image == "" && t.Spec.Source != nil && t.Spec.Source.Image != nil {
		out.Image = t.Spec.Source.Image.Ref
	}
	if at, err := time.Parse(time.RFC3339, t.Annotations[controllers.AnnotationScheduledAt]); err == nil {
		out.ScheduledAt = &at
	}
	for _, e := range t.Spec.EnvOverrides {
		out.Overrides = append(out.Overrides, e.Name)
	}
	if t.Spec.OnSuccess != nil {
		out.Restart = append(out.Restart, t.Spec.OnSuccess.Restart...)
	}
	if c := meta.FindStatusCondition(t.Status.Conditions, controllers.ConditionReady); c != nil {
		out.Reason, out.Message = c.Reason, c.Message
	}
	return out
}

func taskNotFound(project, name string) string {
	return fmt.Sprintf("Task %q in project %q not found.", name, project)
}

func taskObject(t *kwerftv1.Task) *kwerftv1.Task {
	out := t.DeepCopy()
	stripObject(out, "Task")
	return out
}

// taskList lists runs, newest first. Filters: project, app (fromApp),
// schedule, phase, and limit.
func (a *api) taskList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	var err error
	q := r.URL.Query()
	opts := inProject(r)
	if s := q.Get("schedule"); s != "" {
		opts = append(opts, client.MatchingLabels{controllers.LabelSchedule: s})
	}
	limit := 0
	if s := q.Get("limit"); s != "" {
		if limit, err = strconv.Atoi(s); err != nil || limit < 0 {
			invalid(w, "limit", "Limit is a whole number, 0 for all.")
			return
		}
	}
	app, phase := q.Get("app"), strings.ToLower(q.Get("phase"))
	out := []taskJSON{}
	if !a.visitClusters(w, r, ctx, p, "task.list", "No tasks found.", func(v *clusterVisit) error {
		var list kwerftv1.TaskList
		if err := a.scopedList(v.ctx, v.c, v.scope, &list, opts...); err != nil {
			return err
		}
		for i := range list.Items {
			t := &list.Items[i]
			if (app != "" && t.Spec.FromApp != app) || (phase != "" && taskPhase(t) != phase) {
				continue
			}
			s := taskSummary(t)
			s.Cluster = v.conn.name
			out = append(out, s)
		}
		return nil
	}) {
		return
	}
	slices.SortFunc(out, func(x, y taskJSON) int {
		if n := y.Created.Compare(x.Created); n != 0 {
			return n
		}
		return strings.Compare(y.Name, x.Name)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) taskGet(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("task")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var t kwerftv1.Task
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &t); err != nil {
		a.kubeError(w, r, p, "task.get", appTarget(project, name), taskNotFound(project, name), err)
		return
	}
	writeJSON(w, http.StatusOK, taskObject(&t))
}

// validateTaskSpec catches what the CRD lets through but the reconciler
// would wait on forever or trip over: bad variable names and references to
// Apps or Volumes that do not exist. prefix is the spec's field path.
func validateTaskSpec(ctx context.Context, w http.ResponseWriter, c client.Client, project, prefix string, spec *kwerftv1.TaskSpec) bool {
	for _, list := range []struct {
		name string
		vars []corev1.EnvVar
	}{{"env", spec.Env}, {"envOverrides", spec.EnvOverrides}} {
		for i, e := range list.vars {
			if errs := validation.IsEnvVarName(e.Name); len(errs) > 0 {
				invalid(w, fmt.Sprintf("%s.%s[%d].name", prefix, list.name, i), fmt.Sprintf("%q is not a valid variable name. Use letters, digits, _, - and ., not starting with a digit.", e.Name))
				return false
			}
		}
	}
	exists := func(obj client.Object, name string) (bool, error) {
		err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, obj)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	// Errors other than "not found" (a missing project, RBAC) are left to the
	// create itself, which reports them the usual way.
	if spec.FromApp != "" {
		if ok, err := exists(&kwerftv1.App{}, spec.FromApp); err == nil && !ok {
			invalid(w, prefix+".fromApp", fmt.Sprintf("There is no app %q in project %q.", spec.FromApp, project))
			return false
		}
	}
	for i, v := range spec.Volumes {
		if v.Volume == "" {
			continue
		}
		if ok, err := exists(&kwerftv1.Volume{}, v.Volume); err == nil && !ok {
			invalid(w, fmt.Sprintf("%s.volumes[%d].volume", prefix, i), fmt.Sprintf("There is no volume %q in project %q.", v.Volume, project))
			return false
		}
	}
	if spec.OnSuccess != nil {
		for i, name := range spec.OnSuccess.Restart {
			if ok, err := exists(&kwerftv1.App{}, name); err == nil && !ok {
				invalid(w, fmt.Sprintf("%s.onSuccess.restart[%d]", prefix, i), fmt.Sprintf("There is no app %q in project %q to restart.", name, project))
				return false
			}
		}
	}
	return true
}

var notNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// generatedPrefix makes a generateName prefix from base that leaves room for
// suffix and the five random characters the API server appends, within the
// 63 characters a Task (and its Job) name may have.
func generatedPrefix(base, suffix string) string {
	base = strings.Trim(notNameChars.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = "task-" + base
	}
	base = strings.TrimRight(truncate(base, 63-5-len(suffix)), "-")
	return base + suffix
}

// imageName is the last path element of an image reference without its tag
// or digest: ghcr.io/acme/ops:1.4 → ops.
func imageName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	ref, _, _ = strings.Cut(ref, ":")
	return ref
}

// taskCreate starts a one-off run. "Run now" from an App is a Task with
// spec.fromApp and spec.envOverrides; leave out name to get a generated one.
func (a *api) taskCreate(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req struct {
		Name string            `json:"name"`
		Spec kwerftv1.TaskSpec `json:"spec"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Name != "" {
		if errs := validation.IsDNS1123Label(req.Name); len(errs) > 0 {
			invalid(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 63).")
			return
		}
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !validateTaskSpec(ctx, w, c, project, "spec", &req.Spec) {
		return
	}
	t := &kwerftv1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name, Namespace: project,
			Annotations: map[string]string{kwerftv1.AnnotationStartedBy: p.user.Email},
		},
		Spec: req.Spec,
	}
	if t.Name == "" {
		base := req.Spec.FromApp
		if base == "" && req.Spec.Source != nil && req.Spec.Source.Image != nil {
			base = imageName(req.Spec.Source.Image.Ref)
		}
		t.GenerateName = generatedPrefix(base, "-run-")
	}
	if err := c.Create(ctx, t); err != nil {
		a.kubeError(w, r, p, "task.create", appTarget(project, req.Name), taskNotFound(project, req.Name), err)
		return
	}
	a.audit(r, p.user.Email, "task.create", appTarget(project, t.Name), describeTask(t))
	writeJSON(w, http.StatusCreated, taskObject(t))
}

func describeTask(t *kwerftv1.Task) string {
	var parts []string
	switch {
	case t.Spec.FromApp != "":
		parts = append(parts, "from app "+t.Spec.FromApp)
	case t.Spec.Source != nil && t.Spec.Source.Image != nil:
		parts = append(parts, "image "+t.Spec.Source.Image.Ref)
	}
	if s := t.Labels[controllers.LabelSchedule]; s != "" {
		parts = append(parts, "schedule "+s)
	}
	if len(t.Spec.EnvOverrides) > 0 {
		var names []string
		for _, e := range t.Spec.EnvOverrides {
			names = append(names, e.Name)
		}
		parts = append(parts, "overrides "+strings.Join(names, ","))
	}
	return strings.Join(parts, "; ")
}

// taskCancel asks the Task reconciler to stop an unfinished run (see
// kwerftv1.AnnotationCancelRequested). The Task and its record stay.
func (a *api) taskCancel(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("task")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	var t kwerftv1.Task
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &t); err != nil {
		a.kubeError(w, r, p, "task.cancel", target, taskNotFound(project, name), err)
		return
	}
	if t.Status.Phase.Finished() {
		writeError(w, http.StatusConflict, "This run has already finished.")
		return
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{kwerftv1.AnnotationCancelRequested: p.user.Email},
	}})
	if err := c.Patch(ctx, &t, client.RawPatch(types.MergePatchType, patch)); err != nil {
		a.kubeError(w, r, p, "task.cancel", target, taskNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "task.cancel", target, "")
	writeJSON(w, http.StatusOK, taskObject(&t))
}

func (a *api) taskDelete(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("task")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	t := &kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}
	if err := c.Delete(ctx, t, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		a.kubeError(w, r, p, "task.delete", target, taskNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "task.delete", target, "")
	w.WriteHeader(http.StatusNoContent)
}

// ---- schedules ---------------------------------------------------------------

type scheduleJSON struct {
	Name          string     `json:"name"`
	Project       string     `json:"project"`
	Cluster       string     `json:"cluster,omitempty"`
	Schedule      string     `json:"schedule"`
	TimeZone      string     `json:"timeZone,omitempty"`
	Suspend       bool       `json:"suspend"`
	Concurrency   string     `json:"concurrency"`
	FromApp       string     `json:"fromApp,omitempty"`
	Image         string     `json:"image,omitempty"`
	Restart       []string   `json:"restart"`
	NextRun       *time.Time `json:"nextRun,omitempty"`
	LastScheduled *time.Time `json:"lastScheduled,omitempty"`
	LastSuccess   *time.Time `json:"lastSuccess,omitempty"`
	LastFailure   *time.Time `json:"lastFailure,omitempty"`
	Active        []string   `json:"active"`
	LastRun       *taskJSON  `json:"lastRun,omitempty"`
	Phase         string     `json:"phase"` // scheduled | suspended | waiting | pending | failed
	Reason        string     `json:"reason,omitempty"`
	Message       string     `json:"message,omitempty"`
	Created       time.Time  `json:"created"`
}

func scheduleSummary(s *kwerftv1.Schedule, lastRun *kwerftv1.Task) scheduleJSON {
	out := scheduleJSON{
		Name: s.Name, Project: s.Namespace, Schedule: s.Spec.Schedule, TimeZone: s.Spec.TimeZone, Suspend: s.Spec.Suspend,
		Concurrency: s.Spec.Concurrency, FromApp: s.Spec.Task.FromApp, Restart: []string{}, Active: s.Status.Active,
		NextRun: timePtr(s.Status.NextScheduleTime), LastScheduled: timePtr(s.Status.LastScheduleTime),
		LastSuccess: timePtr(s.Status.LastSuccessTime), LastFailure: timePtr(s.Status.LastFailureTime),
		Created: s.CreationTimestamp.UTC(), Phase: "pending",
	}
	if out.Concurrency == "" {
		out.Concurrency = kwerftv1.ConcurrencyForbid
	}
	if out.Active == nil {
		out.Active = []string{}
	}
	if src := s.Spec.Task.Source; src != nil && src.Image != nil {
		out.Image = src.Image.Ref
	}
	if o := s.Spec.Task.OnSuccess; o != nil {
		out.Restart = append(out.Restart, o.Restart...)
	}
	if lastRun != nil {
		sum := taskSummary(lastRun)
		out.LastRun = &sum
	}
	c := meta.FindStatusCondition(s.Status.Conditions, controllers.ConditionReady)
	if c != nil {
		out.Reason, out.Message = c.Reason, c.Message
	}
	switch {
	case c == nil || c.ObservedGeneration < s.Generation:
		if s.Spec.Suspend {
			out.Phase = "suspended"
		}
	case c.Status != metav1.ConditionTrue:
		out.Phase = "failed"
	case c.Reason == "Suspended":
		out.Phase = "suspended"
	case c.Reason == "WaitingForActiveRun":
		out.Phase = "waiting"
	default:
		out.Phase = "scheduled"
	}
	if s.Spec.Suspend {
		out.NextRun = nil
	}
	return out
}

func scheduleNotFound(project, name string) string {
	return fmt.Sprintf("Schedule %q in project %q not found.", name, project)
}

func scheduleObject(s *kwerftv1.Schedule) *kwerftv1.Schedule {
	out := s.DeepCopy()
	stripObject(out, "Schedule")
	return out
}

func (a *api) scheduleList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	out := []scheduleJSON{}
	if !a.visitClusters(w, r, ctx, p, "schedule.list", "No schedules found.", func(v *clusterVisit) error {
		var list kwerftv1.ScheduleList
		if err := a.scopedList(v.ctx, v.c, v.scope, &list, inProject(r)...); err != nil {
			return err
		}
		var tasks kwerftv1.TaskList
		if err := a.scopedList(v.ctx, v.c, v.scope, &tasks, append(inProject(r), client.HasLabels{controllers.LabelSchedule})...); err != nil {
			return err
		}
		latest := map[types.NamespacedName]*kwerftv1.Task{}
		for i := range tasks.Items {
			t := &tasks.Items[i]
			key := types.NamespacedName{Namespace: t.Namespace, Name: t.Labels[controllers.LabelSchedule]}
			if cur := latest[key]; cur == nil || cur.CreationTimestamp.Before(&t.CreationTimestamp) ||
				(cur.CreationTimestamp.Equal(&t.CreationTimestamp) && cur.Name < t.Name) {
				latest[key] = t
			}
		}
		for i := range list.Items {
			s := &list.Items[i]
			last := latest[types.NamespacedName{Namespace: s.Namespace, Name: s.Name}]
			if last != nil && !metav1.IsControlledBy(last, s) {
				last = nil
			}
			sum := scheduleSummary(s, last)
			sum.Cluster = v.conn.name
			if sum.LastRun != nil {
				sum.LastRun.Cluster = v.conn.name
			}
			out = append(out, sum)
		}
		return nil
	}) {
		return
	}
	slices.SortFunc(out, func(x, y scheduleJSON) int { return byProjectAndName(x.Project, x.Name, y.Project, y.Name) })
	writeJSON(w, http.StatusOK, out)
}

// schedulePreview checks a cron expression and time zone with the
// reconciler's own parser and returns the next runs, for the schedule form.
func (a *api) schedulePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	expr, tz := strings.TrimSpace(q.Get("schedule")), strings.TrimSpace(q.Get("timeZone"))
	if !validCron(w, "schedule", "timeZone", expr, tz) {
		return
	}
	sched, loc, _ := controllers.ParseSchedule(expr, tz)
	next := make([]string, 0, 3)
	t := a.now().In(loc)
	for range 3 {
		if t = sched.Next(t); t.IsZero() {
			break
		}
		next = append(next, t.Format(time.RFC3339))
	}
	writeJSON(w, http.StatusOK, map[string]any{"next": next, "timeZone": loc.String()})
}

// validCron reports a bad schedule or time zone on the given fields.
func validCron(w http.ResponseWriter, scheduleField, tzField, expr, tz string) bool {
	if expr == "" {
		invalid(w, scheduleField, "Enter a schedule, like 30 3 * * * (daily at 03:30).")
		return false
	}
	if _, _, err := controllers.ParseSchedule(expr, ""); err != nil {
		invalid(w, scheduleField, cronMessage(err.Error()))
		return false
	}
	if _, _, err := controllers.ParseSchedule(expr, tz); err != nil {
		invalid(w, tzField, fmt.Sprintf("%q is not a known time zone. Use a name like Europe/Berlin.", tz))
		return false
	}
	return true
}

// cronMessage turns the parser's error into a sentence for the form.
func cronMessage(msg string) string {
	if strings.Contains(msg, "timeZone") {
		return "Set the time zone in its own field, not in the schedule."
	}
	if _, after, ok := strings.Cut(msg, ": "); ok {
		msg = after
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	return msg + ". Use five fields: minute hour day-of-month month day-of-week, or @daily, @hourly, @every 2h."
}

func (a *api) scheduleGet(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("schedule")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var s kwerftv1.Schedule
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &s); err != nil {
		a.kubeError(w, r, p, "schedule.get", appTarget(project, name), scheduleNotFound(project, name), err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleObject(&s))
}

func validateScheduleSpec(ctx context.Context, w http.ResponseWriter, c client.Client, project string, spec *kwerftv1.ScheduleSpec) bool {
	spec.Schedule, spec.TimeZone = strings.TrimSpace(spec.Schedule), strings.TrimSpace(spec.TimeZone)
	return validCron(w, "spec.schedule", "spec.timeZone", spec.Schedule, spec.TimeZone) &&
		validateTaskSpec(ctx, w, c, project, "spec.task", &spec.Task)
}

func (a *api) scheduleCreate(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req struct {
		Name string                `json:"name"`
		Spec kwerftv1.ScheduleSpec `json:"spec"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if errs := validation.IsDNS1123Label(req.Name); len(errs) > 0 || len(req.Name) > 52 {
		invalid(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 52: run names add a suffix).")
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !validateScheduleSpec(ctx, w, c, project, &req.Spec) {
		return
	}
	s := &kwerftv1.Schedule{ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: project}, Spec: req.Spec}
	target := appTarget(project, req.Name)
	if err := c.Create(ctx, s); err != nil {
		a.kubeError(w, r, p, "schedule.create", target, scheduleNotFound(project, req.Name), err)
		return
	}
	a.audit(r, p.user.Email, "schedule.create", target, s.Spec.Schedule)
	writeJSON(w, http.StatusCreated, scheduleObject(s))
}

// scheduleUpdate replaces the spec, guarded by the generation that was
// edited (or the resourceVersion), like appUpdate.
func (a *api) scheduleUpdate(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("schedule")
	var req struct {
		Spec            kwerftv1.ScheduleSpec `json:"spec"`
		Generation      int64                 `json:"generation"`
		ResourceVersion string                `json:"resourceVersion"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !validateScheduleSpec(ctx, w, c, project, &req.Spec) {
		return
	}
	target := appTarget(project, name)
	var s kwerftv1.Schedule
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &s); err != nil {
		a.kubeError(w, r, p, "schedule.update", target, scheduleNotFound(project, name), err)
		return
	}
	if req.Generation != 0 && req.Generation != s.Generation {
		writeError(w, http.StatusConflict, "Someone else changed this schedule in the meantime. Reload and try again.")
		return
	}
	if req.ResourceVersion != "" {
		s.ResourceVersion = req.ResourceVersion
	}
	s.Spec = req.Spec
	if err := c.Update(ctx, &s); err != nil {
		a.kubeError(w, r, p, "schedule.update", target, scheduleNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "schedule.update", target, fmt.Sprintf("generation %d", s.Generation))
	writeJSON(w, http.StatusOK, scheduleObject(&s))
}

// scheduleDelete removes the Schedule; its Tasks (and their Jobs) are
// garbage-collected with it.
func (a *api) scheduleDelete(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("schedule")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	s := &kwerftv1.Schedule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}
	if err := c.Delete(ctx, s, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		a.kubeError(w, r, p, "schedule.delete", target, scheduleNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "schedule.delete", target, "")
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) scheduleSuspend(suspend bool) http.HandlerFunc {
	action := "schedule.resume"
	if suspend {
		action = "schedule.suspend"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		project, name := r.PathValue("project"), r.PathValue("schedule")
		c, p, ctx, cancel, err := a.userClient(r)
		defer cancel()
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		target := appTarget(project, name)
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"suspend": suspend}})
		s := &kwerftv1.Schedule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}
		if err := c.Patch(ctx, s, client.RawPatch(types.MergePatchType, patch)); err != nil {
			a.kubeError(w, r, p, action, target, scheduleNotFound(project, name), err)
			return
		}
		a.audit(r, p.user.Email, action, target, "")
		writeJSON(w, http.StatusOK, scheduleObject(s))
	}
}

// scheduleRun starts a run of the Schedule now, from its task template with
// optional env overrides. The Task carries the schedule label and the
// Schedule as its controller, exactly like a scheduled run, so it shows up
// in the Schedule's history, counts for concurrency and is pruned with it.
// blockOwnerDeletion stays unset: setting it would need update rights on
// schedules/finalizers where OwnerReferencesPermissionEnforcement is on, and
// background deletion of the Schedule collects the Task either way.
func (a *api) scheduleRun(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("schedule")
	var req struct {
		EnvOverrides []corev1.EnvVar `json:"envOverrides"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	for i, e := range req.EnvOverrides {
		if errs := validation.IsEnvVarName(e.Name); len(errs) > 0 {
			invalid(w, fmt.Sprintf("envOverrides[%d].name", i), fmt.Sprintf("%q is not a valid variable name. Use letters, digits, _, - and ., not starting with a digit.", e.Name))
			return
		}
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	var s kwerftv1.Schedule
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &s); err != nil {
		a.kubeError(w, r, p, "schedule.run", target, scheduleNotFound(project, name), err)
		return
	}
	spec := s.Spec.Task.DeepCopy()
	spec.EnvOverrides = mergeEnv(spec.EnvOverrides, req.EnvOverrides)
	t := &kwerftv1.Task{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: generatedPrefix(s.Name, "-manual-"),
			Namespace:    project,
			Labels:       map[string]string{controllers.LabelSchedule: s.Name, controllers.LabelManagedBy: controllers.ManagedByKwerft},
			Annotations:  map[string]string{kwerftv1.AnnotationStartedBy: p.user.Email},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kwerftv1.GroupVersion.String(),
				Kind:       "Schedule",
				Name:       s.Name,
				UID:        s.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: *spec,
	}
	if err := c.Create(ctx, t); err != nil {
		a.kubeError(w, r, p, "schedule.run", target, scheduleNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "schedule.run", target, "task "+t.Name+overridesDetail(req.EnvOverrides))
	writeJSON(w, http.StatusCreated, taskObject(t))
}

func overridesDetail(env []corev1.EnvVar) string {
	if len(env) == 0 {
		return ""
	}
	names := make([]string, 0, len(env))
	for _, e := range env {
		names = append(names, e.Name)
	}
	return "; overrides " + strings.Join(names, ",")
}

// mergeEnv returns base with over applied by name: replaced in place, or
// appended in order.
func mergeEnv(base, over []corev1.EnvVar) []corev1.EnvVar {
	out := slices.Clone(base)
	for _, e := range over {
		if i := slices.IndexFunc(out, func(x corev1.EnvVar) bool { return x.Name == e.Name }); i >= 0 {
			out[i] = e
		} else {
			out = append(out, e)
		}
	}
	return out
}

// ---- domains -----------------------------------------------------------------

type domainJSON struct {
	Name        string     `json:"name"`
	Project     string     `json:"project"`
	Cluster     string     `json:"cluster,omitempty"`
	Hostname    string     `json:"hostname"`
	App         string     `json:"app,omitempty"` // the App that claimed it, if any
	Listener    string     `json:"listener,omitempty"`
	Certificate string     `json:"certificate"` // valid | issuing | pending | failed | disabled
	Reason      string     `json:"reason,omitempty"`
	Message     string     `json:"message,omitempty"`
	NotAfter    *time.Time `json:"notAfter,omitempty"`
	Created     time.Time  `json:"created"`
}

func domainSummary(d *kwerftv1.Domain) domainJSON {
	out := domainJSON{
		Name: d.Name, Project: d.Namespace, Hostname: d.Spec.Hostname, Listener: d.Status.Listener,
		NotAfter: timePtr(d.Status.NotAfter), Created: d.CreationTimestamp.UTC(), Certificate: "pending",
	}
	if owner := metav1.GetControllerOf(d); owner != nil && owner.Kind == "App" {
		out.App = owner.Name
	}
	c := meta.FindStatusCondition(d.Status.Conditions, controllers.ConditionReady)
	if c == nil {
		return out
	}
	out.Reason, out.Message = c.Reason, c.Message
	switch {
	case c.ObservedGeneration < d.Generation:
	case c.Status == metav1.ConditionTrue:
		out.Certificate = "valid"
	case c.Reason == "CertificateIssuing" || c.Reason == "CertificatePending" || c.Reason == "CertificateUnknown":
		out.Certificate = "issuing"
	case c.Reason == "CertificatesDisabled":
		out.Certificate = "disabled"
	default:
		out.Certificate = "failed"
	}
	return out
}

func (a *api) domainList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	out := []domainJSON{}
	if !a.visitClusters(w, r, ctx, p, "domain.list", "No domains found.", func(v *clusterVisit) error {
		var list kwerftv1.DomainList
		if err := a.scopedList(v.ctx, v.c, v.scope, &list, inProject(r)...); err != nil {
			return err
		}
		for i := range list.Items {
			s := domainSummary(&list.Items[i])
			s.Cluster = v.conn.name
			out = append(out, s)
		}
		return nil
	}) {
		return
	}
	slices.SortFunc(out, func(x, y domainJSON) int {
		if n := strings.Compare(x.Hostname, y.Hostname); n != 0 {
			return n
		}
		return byProjectAndName(x.Project, x.Name, y.Project, y.Name)
	})
	writeJSON(w, http.StatusOK, out)
}
