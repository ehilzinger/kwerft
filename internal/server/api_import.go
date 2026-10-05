package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/compose"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/importplan"
	"github.com/ehilzinger/kwerft/internal/templates"
)

// Compose import and templates (docs/phase6.md › Compose import and
// templates). Both turn into an importplan.Plan — Volumes, SecretSets, Apps
// — which is checked and applied here, always as the signed-in user:
//
//   - Every App, set and Volume first goes through the checks of the
//     endpoints that create them one by one (validateSpec, validateSetSpec,
//     the Volume checks), then through a server-side dry run as the user:
//     RBAC, the CRDs' schemas and names already taken answer before
//     anything exists. A dry run ({dryRun: true}) stops there and returns
//     the plan with the problems found.
//   - Applying creates the objects in order (Volumes, shared sets, Apps,
//     each App's own set and its values), and deletes what it created, in
//     reverse, if a later step fails: an import happens completely or not.
//
// Values from a Compose file reach Kubernetes only as a write to the App's
// own set's Secret (writeKeys, like the env editor); no answer carries them.

// importTimeout bounds an import: a value write may wait up to
// secretWriteWait for a new set's Secret, once per set.
const importTimeout = 90 * time.Second

func (a *api) registerImport(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	mux.HandleFunc("GET /api/v1/templates", a.requireUser(a.templateList))
	mux.HandleFunc("POST /api/v1/projects/{project}/import/compose", write(a.importCompose))
	mux.HandleFunc("POST /api/v1/projects/{project}/templates/{template}", write(a.templateApply))
}

// problemJSON is something that stops a plan: the object, the field of it
// (as the single-object endpoints name it) and why.
type problemJSON struct {
	Object  string `json:"object"` // Kind/name
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	// exists: the name is taken (409 rather than 422 on apply).
	exists bool
}

type importJSON struct {
	*importplan.Plan
	DryRun   bool          `json:"dryRun"`
	Problems []problemJSON `json:"problems"`
	// Created lists the objects an apply made, as Kind/name.
	Created []string `json:"created"`
}

// ---- templates ---------------------------------------------------------------

func (a *api) templateList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, templates.List())
}

func (a *api) templateApply(w http.ResponseWriter, r *http.Request) {
	tpl := templates.Get(r.PathValue("template"))
	if tpl == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("There is no template %q.", r.PathValue("template")))
		return
	}
	var req struct {
		Parameters map[string]string `json:"parameters"`
		DryRun     bool              `json:"dryRun"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	c, p, ctx, cancel, err := a.importClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	plan, err := tpl.Render(req.Parameters, a.appsDomainFor(ctx, c, p))
	var fe *templates.FieldError
	switch {
	case errors.As(err, &fe):
		invalid(w, fe.Field, fe.Message)
		return
	case err != nil:
		a.internalError(w, r, err)
		return
	}
	a.runPlan(w, r, ctx, c, p, plan, req.DryRun, "template.apply", "template "+tpl.ID)
}

// ---- Compose -----------------------------------------------------------------

func (a *api) importCompose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Compose string `json:"compose"`
		// Env holds the values of the file's ${VAR} references (a .env file).
		Env    map[string]string `json:"env"`
		DryRun bool              `json:"dryRun"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	c, p, ctx, cancel, err := a.importClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	plan, err := compose.Convert([]byte(req.Compose), compose.Options{AppsDomain: a.appsDomainFor(ctx, c, p), Env: req.Env})
	if err != nil {
		invalid(w, "compose", err.Error())
		return
	}
	a.runPlan(w, r, ctx, c, p, plan, req.DryRun, "import.compose", "")
}

// importClient is userClient with a deadline long enough for an import.
func (a *api) importClient(r *http.Request) (client.Client, *principal, context.Context, context.CancelFunc, error) {
	p := principalOf(r)
	c, err := a.conn(r.Context()).kube.For(p.user.Email, p.user.Role)
	ctx, cancel := context.WithTimeout(r.Context(), importTimeout)
	return c, p, ctx, cancel, err
}

// appsDomainFor is the apps domain public hostnames go under: the project's
// cluster's, else the console's (whose records it keeps for remote
// clusters), as the deploy wizard suggests them. Everyone signed in may
// read the settings.
func (a *api) appsDomainFor(ctx context.Context, c client.Client, p *principal) string {
	var cs kwerftv1.ConsoleSettings
	if err := c.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cs); err == nil && cs.Spec.AppsDomain != "" {
		return cs.Spec.AppsDomain
	}
	if a.conn(ctx).isLocal() {
		return ""
	}
	lc, err := a.managementClient(p)
	if err != nil {
		return ""
	}
	if err := lc.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cs); err != nil {
		return ""
	}
	return cs.Spec.AppsDomain
}

// ---- checking and applying a plan --------------------------------------------------

// fieldRecorder catches the answer of a single-object validator (which
// writes its 422 itself), so a plan is checked by exactly the same code.
type fieldRecorder struct {
	header http.Header
	body   []byte
}

func (f *fieldRecorder) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *fieldRecorder) Write(b []byte) (int, error) {
	f.body = append(f.body, b...)
	return len(b), nil
}
func (f *fieldRecorder) WriteHeader(int) {}

func recorded(object string, ok func(w http.ResponseWriter) bool) *problemJSON {
	rec := &fieldRecorder{}
	if ok(rec) {
		return nil
	}
	var body struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}
	_ = json.Unmarshal(rec.body, &body)
	return &problemJSON{Object: object, Field: body.Field, Message: body.Error}
}

// check runs the checks the endpoints for one App, set or Volume run.
func (a *api) check(ctx context.Context, plan *importplan.Plan) []problemJSON {
	var out []problemJSON
	add := func(p *problemJSON) {
		if p != nil {
			out = append(out, *p)
		}
	}
	for i := range plan.Volumes {
		v := &plan.Volumes[i]
		obj := "Volume/" + v.Name
		switch {
		case len(validation.IsDNS1123Label(v.Name)) > 0:
			add(&problemJSON{Object: obj, Field: "name", Message: "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 63)."})
		case v.Spec.Size.Sign() <= 0:
			add(&problemJSON{Object: obj, Field: "size", Message: "Enter a size such as 10Gi or 500Mi."})
		case v.Spec.Class == "hcloud-volume" && a.cfg.StorageClassExists != nil && !a.cfg.StorageClassExists(ctx, HCloudVolumesClass):
			add(&problemJSON{Object: obj, Field: "class", Message: "Hetzner Cloud Volumes are not set up on this cluster. Choose local-nvme (x-kwerft: {class: local-nvme}), or set them up under Settings › Hetzner Cloud API."})
		}
	}
	for i := range plan.SecretSets {
		s := &plan.SecretSets[i]
		obj := "SecretSet/" + s.Name
		if !validSetName(s.Name) {
			add(&problemJSON{Object: obj, Field: "name", Message: "Use lowercase letters, digits, - and ., starting and ending with a letter or digit (at most 63)."})
			continue
		}
		spec := kwerftv1.SecretSetSpec{Description: s.Description, Generate: s.Generate, Derived: s.Derived}
		add(recorded(obj, func(w http.ResponseWriter) bool { return validateSetSpec(w, &spec) }))
		for k := range s.Values {
			if !controllers.SecretKeyRE.MatchString(k) {
				add(&problemJSON{Object: obj, Field: "keys", Message: fmt.Sprintf("%q is not a valid key.", k)})
			} else if len(s.Values[k]) > maxSecretValueSize {
				add(&problemJSON{Object: obj, Field: "keys", Message: k + " is larger than 60 KiB."})
			}
		}
	}
	for i := range plan.Apps {
		app := &plan.Apps[i]
		obj := "App/" + app.Name
		if !importplan.ValidAppName(app.Name) {
			add(&problemJSON{Object: obj, Field: "name", Message: fmt.Sprintf("Use lowercase letters, digits and dashes, starting with a letter (at most %d).", importplan.MaxAppName)})
			continue
		}
		add(recorded(obj, func(w http.ResponseWriter) bool { return validateSpec(w, &app.Spec) }))
	}
	return out
}

// planObjects are the plan's objects in creation order. Values are the
// own sets' values, written after the set.
type planObject struct {
	obj    client.Object
	kind   string
	owner  string // an App's own set: the App
	values map[string]string
}

func objectsOf(project string, plan *importplan.Plan) []planObject {
	var out []planObject
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: project} }
	for _, v := range plan.Volumes {
		out = append(out, planObject{kind: "Volume", obj: &kwerftv1.Volume{ObjectMeta: meta(v.Name), Spec: v.Spec}})
	}
	set := func(s importplan.SecretSet) planObject {
		return planObject{kind: "SecretSet", owner: s.App, values: s.Values, obj: &kwerftv1.SecretSet{ObjectMeta: meta(s.Name),
			Spec: kwerftv1.SecretSetSpec{Description: s.Description, Generate: s.Generate, Derived: s.Derived}}}
	}
	for _, s := range plan.SecretSets {
		if s.App == "" {
			out = append(out, set(s))
		}
	}
	for _, app := range plan.Apps {
		out = append(out, planObject{kind: "App", obj: &kwerftv1.App{ObjectMeta: meta(app.Name), Spec: app.Spec}})
	}
	for _, s := range plan.SecretSets {
		if s.App != "" {
			out = append(out, set(s))
		}
	}
	return out
}

func (o planObject) name() string { return o.kind + "/" + o.obj.GetName() }

// runPlan checks the plan, and applies it unless dryRun.
func (a *api) runPlan(w http.ResponseWriter, r *http.Request, ctx context.Context, c client.Client, p *principal,
	plan *importplan.Plan, dryRun bool, action, detail string) {
	project := r.PathValue("project")
	plan.Normalize()
	out := importJSON{Plan: plan, DryRun: dryRun, Problems: a.check(ctx, plan), Created: []string{}}
	if len(out.Problems) == 0 {
		problems, ok := a.dryRun(w, r, ctx, c, p, project, plan, action)
		if !ok {
			return
		}
		out.Problems = problems
	}
	if dryRun {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(out.Problems) > 0 {
		// Nothing was created: the same plan, with what stops it.
		status := http.StatusUnprocessableEntity
		if slices.ContainsFunc(out.Problems, func(pr problemJSON) bool { return pr.exists }) {
			status = http.StatusConflict
		}
		writeJSON(w, status, struct {
			importJSON
			Error string `json:"error"`
		}{out, "Nothing was created: " + out.Problems[0].Object + ": " + out.Problems[0].Message})
		return
	}
	created, err := a.apply(ctx, r, c, p, project, plan)
	if err != nil {
		left := a.rollback(r, c, p, project, created, action)
		failed := "the import"
		var se *stepError
		if errors.As(err, &se) {
			failed, err = se.object, se.err
		}
		msg := fmt.Sprintf("Creating %s failed, so the import was undone: %s", failed, importErrorText(err))
		if len(left) > 0 {
			msg = fmt.Sprintf("Creating %s failed (%s), and %s could not be removed again; delete them by hand.", failed, importErrorText(err), strings.Join(left, ", "))
		}
		a.audit(r, p.user.Email, action+".failed", project, strings.TrimSpace(detail+" "+failed+": "+importErrorText(err)))
		status := http.StatusBadGateway
		switch {
		case apierrors.IsForbidden(err):
			status = http.StatusForbidden
		case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
			status = http.StatusConflict
		case apierrors.IsInvalid(err):
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, msg)
		return
	}
	out.Created = created
	a.audit(r, p.user.Email, action, project, strings.TrimSpace(detail+" "+summarize(plan)))
	writeJSON(w, http.StatusCreated, out)
}

func summarize(plan *importplan.Plan) string {
	count := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	return fmt.Sprintf("(%s, %s, %s)", count(len(plan.Apps), "app", "apps"), count(len(plan.Volumes), "volume", "volumes"),
		count(len(plan.SecretSets), "secret set", "secret sets"))
}

// dryRun creates every object with dryRun=All, as the user: Kubernetes
// answers for RBAC, the schemas and names already taken without storing
// anything. A refusal by RBAC answers the request (403); everything else
// becomes a problem of the plan. On false it has answered.
func (a *api) dryRun(w http.ResponseWriter, r *http.Request, ctx context.Context, c client.Client, p *principal,
	project string, plan *importplan.Plan, action string) ([]problemJSON, bool) {
	var problems []problemJSON
	uids := map[string]types.UID{}
	for _, o := range objectsOf(project, plan) {
		obj := o.obj.DeepCopyObject().(client.Object)
		if o.owner != "" {
			uid := uids[o.owner]
			if uid == "" {
				uid = "00000000-0000-0000-0000-000000000000" // its App failed the dry run too
			}
			obj.SetOwnerReferences([]metav1.OwnerReference{appOwner(&kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: o.owner, UID: uid}})})
		}
		err := c.Create(ctx, obj, client.DryRunAll)
		if o.kind == "App" && err == nil {
			uids[obj.GetName()] = obj.GetUID()
		}
		switch {
		case err == nil:
		case apierrors.IsForbidden(err):
			a.audit(r, p.user.Email, action+".denied", project, "forbidden by Kubernetes RBAC: "+o.name())
			writeError(w, http.StatusForbidden, "Your role does not allow creating apps, volumes and secret sets in this project.")
			return nil, false
		case apierrors.IsAlreadyExists(err):
			problems = append(problems, problemJSON{Object: o.name(), Field: "name", exists: true,
				Message: fmt.Sprintf("%s %s already exists in the project.", o.kind, o.obj.GetName())})
		case apierrors.IsInvalid(err):
			field, msg := invalidCause(err)
			problems = append(problems, problemJSON{Object: o.name(), Field: field, Message: msg})
		default:
			a.kubeError(w, r, p, action, project, fmt.Sprintf("Project %q not found.", project), err)
			return nil, false
		}
	}
	return problems, true
}

func invalidCause(err error) (field, msg string) {
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Details != nil && len(status.Status().Details.Causes) > 0 {
		cause := status.Status().Details.Causes[0]
		return cause.Field, humanize(cause.Field, cause.Message)
	}
	return "", err.Error()
}

func importErrorText(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "your role does not allow this."
	case apierrors.IsInvalid(err):
		field, msg := invalidCause(err)
		if field != "" {
			return field + ": " + msg
		}
		return msg
	case apierrors.IsAlreadyExists(err):
		return "it exists already (created in the meantime)."
	case apierrors.IsNotFound(err):
		return "the set's Secret does not exist yet; try again in a moment."
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "Kubernetes did not answer in time."
	}
	return "Kubernetes refused it (" + string(apierrors.ReasonForError(err)) + ")."
}

// stepError names the object an apply failed at.
type stepError struct {
	object string
	err    error
}

func (e *stepError) Error() string { return e.object + ": " + e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// apply creates the plan's objects as the user, in order, and writes the
// own sets' values. It returns what it created (Kind/name), also on error.
func (a *api) apply(ctx context.Context, r *http.Request, c client.Client, p *principal, project string, plan *importplan.Plan) ([]string, error) {
	created := []string{}
	apps := map[string]*kwerftv1.App{}
	for _, o := range objectsOf(project, plan) {
		target := appTarget(project, o.obj.GetName())
		if o.owner != "" {
			owner := apps[o.owner]
			o.obj.SetOwnerReferences([]metav1.OwnerReference{appOwner(owner)})
		}
		if err := c.Create(ctx, o.obj); err != nil {
			return created, &stepError{object: o.name(), err: err}
		}
		created = append(created, o.name())
		if a.cfg.importFault != nil {
			if err := a.cfg.importFault(o.name()); err != nil {
				return created, &stepError{object: o.name(), err: err}
			}
		}
		switch obj := o.obj.(type) {
		case *kwerftv1.App:
			apps[obj.Name] = obj
			a.audit(r, p.user.Email, "app.create", target, describeSource(obj))
		case *kwerftv1.Volume:
			a.audit(r, p.user.Email, "volume.create", target, fmt.Sprintf("%s %s", obj.Spec.Size.String(), obj.Spec.Class))
		case *kwerftv1.SecretSet:
			detail := ""
			if o.owner != "" {
				detail = "own set of app " + o.owner
			}
			a.audit(r, p.user.Email, "secret_set.create", target, detail)
			if len(o.values) == 0 {
				continue
			}
			data, ann := map[string]any{}, map[string]any{}
			rec := controllers.KeyRecord{At: a.now(), By: p.user.Email, Source: controllers.SourceSet}
			for k, v := range o.values {
				data[k] = []byte(v)
				ann[controllers.KeyAnnotation(k)] = rec.Encode()
			}
			if err := a.writeKeys(ctx, c, obj, data, ann); err != nil {
				return created, &stepError{object: "the values of " + o.name(), err: err}
			}
			for k := range o.values {
				a.audit(r, p.user.Email, "secret.set", secretTarget(project, obj.Name, k), "imported")
			}
		}
	}
	return created, nil
}

// rollback deletes what an apply created, newest first, as the user, and
// returns what it could not delete. It has its own deadline: the request's
// may be what ran out.
func (a *api) rollback(r *http.Request, c client.Client, p *principal, project string, created []string, action string) []string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), kubeTimeout)
	defer cancel()
	var left []string
	for i := len(created) - 1; i >= 0; i-- {
		kind, name, _ := strings.Cut(created[i], "/")
		meta := metav1.ObjectMeta{Name: name, Namespace: project}
		var obj client.Object
		switch kind {
		case "App":
			obj = &kwerftv1.App{ObjectMeta: meta}
		case "Volume":
			obj = &kwerftv1.Volume{ObjectMeta: meta}
		case "SecretSet":
			obj = &kwerftv1.SecretSet{ObjectMeta: meta}
		}
		if err := c.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			a.cfg.Logger.Error("import rollback: could not delete", "project", project, "object", created[i], "err", err)
			left = append(left, created[i])
			continue
		}
		audit := map[string]string{"App": "app.delete", "Volume": "volume.delete", "SecretSet": "secret_set.delete"}[kind]
		a.audit(r, p.user.Email, audit, appTarget(project, name), "rollback of "+action)
	}
	return left
}
