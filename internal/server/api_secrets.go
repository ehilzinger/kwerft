// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/access"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Secret sets (docs/plan.md › Secrets, Phase 6). Everything here acts as
// the signed-in user:
//   - Sets (kwerft.dev SecretSets) are read, created, changed and deleted
//     through the project's RoleBindings like Apps. Their status carries key
//     names, never values, so listing never touches a Secret.
//   - Values are written with patch on the set's Secret, which the
//     SecretSet reconciler's Role kwerft:secret-sets allows owners, admins
//     and the project's developers on exactly the managed Secrets. The patch
//     goes through the metadata endpoint, so the API server answers with the
//     object's metadata only: the values in the stored Secret never reach
//     the console.
//   - Reveal reads one key with get (Role kwerft:secret-sets-read: owners
//     and admins), after the password, never for API tokens. Copy reads the
//     source set the same way and writes the copy like a user setting keys.
//
// Values are never logged, audited or returned, except by reveal. Audit
// entries name the set and key.

// secretSetYoung: a set this new may still wait for the reconciler to
// create its Secret and name it in the Role; writes retry meanwhile.
const (
	secretSetYoung     = 30 * time.Second
	secretWriteWait    = 10 * time.Second
	maxSecretValueSize = 60 << 10
)

func (a *api) registerSecrets(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	const base = "/api/v1/projects/{project}/secret-sets"
	mux.HandleFunc("GET "+base, read(a.secretSetList))
	mux.HandleFunc("POST "+base, write(a.secretSetCreate))
	mux.HandleFunc("GET "+base+"/{set}", read(a.secretSetGet))
	mux.HandleFunc("PATCH "+base+"/{set}", write(a.secretSetUpdate))
	mux.HandleFunc("DELETE "+base+"/{set}", write(a.secretSetDelete))
	mux.HandleFunc("POST "+base+"/{set}/copy", write(a.secretSetCopy))
	mux.HandleFunc("PUT "+base+"/{set}/keys/{key}", write(a.secretKeyPut))
	mux.HandleFunc("DELETE "+base+"/{set}/keys/{key}", write(a.secretKeyDelete))
	mux.HandleFunc("POST "+base+"/{set}/keys/{key}/reveal", write(a.secretKeyReveal))
}

// ---- JSON ------------------------------------------------------------------------

type secretKeyJSON struct {
	Name      string     `json:"name"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	// UpdatedBy is the console user who last wrote it; empty for keys the
	// reconciler generated or derived.
	UpdatedBy string `json:"updatedBy,omitempty"`
	Source    string `json:"source,omitempty"` // Set | Generated | Derived
}

type secretSetJSON struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	Description string `json:"description,omitempty"`
	// App owns the set: it is that App's own (<app>-env), deleted with it.
	App      string                      `json:"app,omitempty"`
	Generate []string                    `json:"generate"`
	Derived  []kwerftv1.DerivedSecretKey `json:"derived"`
	Keys     []secretKeyJSON             `json:"keys"`
	UsedBy   []string                    `json:"usedBy"`
	Missing  []string                    `json:"missing"`
	// Phase: ready, pending (not looked at yet), missing (referenced keys
	// or a derived key's inputs are not set) or failed (Conflict, ...).
	Phase           string    `json:"phase"`
	Reason          string    `json:"reason,omitempty"`
	Message         string    `json:"message,omitempty"`
	Created         time.Time `json:"created"`
	ResourceVersion string    `json:"resourceVersion"`
}

func secretSetSummary(s *kwerftv1.SecretSet) secretSetJSON {
	out := secretSetJSON{
		Name: s.Name, Project: s.Namespace, Description: s.Spec.Description,
		Generate: s.Spec.Generate, Derived: s.Spec.Derived, Keys: []secretKeyJSON{},
		UsedBy: s.Status.UsedBy, Missing: s.Status.Missing,
		Created: s.CreationTimestamp.UTC(), ResourceVersion: s.ResourceVersion, Phase: "pending",
	}
	if out.Generate == nil {
		out.Generate = []string{}
	}
	if out.Derived == nil {
		out.Derived = []kwerftv1.DerivedSecretKey{}
	}
	if out.UsedBy == nil {
		out.UsedBy = []string{}
	}
	if out.Missing == nil {
		out.Missing = []string{}
	}
	if owner := metav1.GetControllerOf(s); owner != nil && owner.Kind == "App" {
		out.App = owner.Name
	}
	for _, k := range s.Status.Keys {
		kj := secretKeyJSON{Name: k.Name, UpdatedBy: k.UpdatedBy, Source: k.Source}
		if k.UpdatedAt != nil {
			t := k.UpdatedAt.UTC()
			kj.UpdatedAt = &t
		}
		out.Keys = append(out.Keys, kj)
	}
	if c := meta.FindStatusCondition(s.Status.Conditions, controllers.ConditionReady); c != nil {
		out.Reason, out.Message = c.Reason, c.Message
		switch {
		case c.ObservedGeneration < s.Generation:
		case c.Status == metav1.ConditionTrue:
			out.Phase = "ready"
		case c.Reason == "MissingKeys" || c.Reason == "DerivedPending":
			out.Phase = "missing"
		default:
			out.Phase = "failed"
		}
	}
	return out
}

func secretSetNotFound(project, name string) string {
	return fmt.Sprintf("Secret set %q in project %q not found.", name, project)
}

func secretTarget(project, set string, key ...string) string {
	return strings.Join(append([]string{project, set}, key...), "/")
}

// ---- validation ------------------------------------------------------------------

func validSetName(name string) bool {
	return len(name) <= 63 && len(validation.IsDNS1123Subdomain(name)) == 0
}

// validateSetSpec checks generate and derived as the reconciler will use
// them. On false it has answered.
func validateSetSpec(w http.ResponseWriter, spec *kwerftv1.SecretSetSpec) bool {
	if len(spec.Description) > 200 {
		invalid(w, "description", "Keep the description under 200 characters.")
		return false
	}
	seen := map[string]bool{}
	for i, k := range spec.Generate {
		if !controllers.SecretKeyRE.MatchString(k) {
			invalid(w, fmt.Sprintf("generate[%d]", i), fmt.Sprintf("%q is not a valid key. Use letters, digits, -, _ and .", k))
			return false
		}
		if seen[k] {
			invalid(w, fmt.Sprintf("generate[%d]", i), k+" is listed twice.")
			return false
		}
		seen[k] = true
	}
	for i, d := range spec.Derived {
		field := fmt.Sprintf("derived[%d]", i)
		if !controllers.SecretKeyRE.MatchString(d.Key) {
			invalid(w, field+".key", fmt.Sprintf("%q is not a valid key. Use letters, digits, -, _ and .", d.Key))
			return false
		}
		if seen[d.Key] {
			invalid(w, field+".key", d.Key+" is listed twice: a key is generated, derived or set, not two of them.")
			return false
		}
		seen[d.Key] = true
		refs := controllers.TemplateKeys(d.Template)
		switch {
		case strings.TrimSpace(d.Template) == "" || len(d.Template) > 2048:
			invalid(w, field+".template", "Enter a template of up to 2048 characters, like postgres://app:${PASSWORD}@db:5432/app.")
			return false
		case len(refs) == 0:
			invalid(w, field+".template", "A derived key refers to other keys of the set as ${KEY}.")
			return false
		case slices.Contains(refs, d.Key):
			invalid(w, field+".template", d.Key+" cannot refer to itself.")
			return false
		}
	}
	return true
}

func derivedKey(spec *kwerftv1.SecretSetSpec, key string) bool {
	return slices.ContainsFunc(spec.Derived, func(d kwerftv1.DerivedSecretKey) bool { return d.Key == key })
}

// ---- sets ------------------------------------------------------------------------

func (a *api) secretSetList(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var list kwerftv1.SecretSetList
	if err := c.List(ctx, &list, client.InNamespace(project)); err != nil {
		a.kubeError(w, r, p, "secret_set.list", project, fmt.Sprintf("Project %q not found.", project), err)
		return
	}
	out := make([]secretSetJSON, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, secretSetSummary(&list.Items[i]))
	}
	slices.SortFunc(out, func(x, y secretSetJSON) int { return strings.Compare(x.Name, y.Name) })
	writeJSON(w, http.StatusOK, out)
}

func (a *api) secretSetGet(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("set")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var set kwerftv1.SecretSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &set); err != nil {
		a.kubeError(w, r, p, "secret_set.get", secretTarget(project, name), secretSetNotFound(project, name), err)
		return
	}
	writeJSON(w, http.StatusOK, secretSetSummary(&set))
}

// secretSetCreate makes a set. With app, the set is that App's own: owned by
// it (deleted with it) and named <app>-env unless a name is given.
func (a *api) secretSetCreate(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req struct {
		Name        string                      `json:"name"`
		Description string                      `json:"description"`
		Generate    []string                    `json:"generate"`
		Derived     []kwerftv1.DerivedSecretKey `json:"derived"`
		App         string                      `json:"app"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Name, req.App, req.Description = strings.TrimSpace(req.Name), strings.TrimSpace(req.App), strings.TrimSpace(req.Description)
	if req.Name == "" && req.App != "" {
		req.Name = controllers.AppEnvSet(req.App)
	}
	if !validSetName(req.Name) {
		invalid(w, "name", "Use lowercase letters, digits, - and ., starting and ending with a letter or digit (at most 63).")
		return
	}
	spec := kwerftv1.SecretSetSpec{Description: req.Description, Generate: req.Generate, Derived: req.Derived}
	if !validateSetSpec(w, &spec) {
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	set := &kwerftv1.SecretSet{ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: project}, Spec: spec}
	if req.App != "" {
		var app kwerftv1.App
		if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: req.App}, &app); err != nil {
			a.kubeError(w, r, p, "secret_set.create", secretTarget(project, req.Name), appNotFound(project, req.App), err)
			return
		}
		set.OwnerReferences = []metav1.OwnerReference{appOwner(&app)}
	}
	target := secretTarget(project, req.Name)
	if err := c.Create(ctx, set); err != nil {
		a.kubeError(w, r, p, "secret_set.create", target, secretSetNotFound(project, req.Name), err)
		return
	}
	detail := ""
	if req.App != "" {
		detail = "own set of app " + req.App
	}
	a.audit(r, p.user.Email, "secret_set.create", target, detail)
	writeJSON(w, http.StatusCreated, secretSetSummary(set))
}

// appOwner makes an App the controller of its own set, so the set (and its
// Secret, which the set owns) is deleted with the App. No
// blockOwnerDeletion: that would need a right on the App's finalizers.
func appOwner(app *kwerftv1.App) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: kwerftv1.GroupVersion.String(), Kind: "App", Name: app.Name, UID: app.UID, Controller: boolPtr(true)}
}

// secretSetUpdate changes the description, the generated keys or the
// derived keys; fields left out stay. resourceVersion (optional) refuses
// the change when the set changed since it was read.
func (a *api) secretSetUpdate(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("set")
	var req struct {
		Description     *string                      `json:"description"`
		Generate        *[]string                    `json:"generate"`
		Derived         *[]kwerftv1.DerivedSecretKey `json:"derived"`
		ResourceVersion string                       `json:"resourceVersion"`
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
	target := secretTarget(project, name)
	var set kwerftv1.SecretSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &set); err != nil {
		a.kubeError(w, r, p, "secret_set.update", target, secretSetNotFound(project, name), err)
		return
	}
	if req.Description != nil {
		set.Spec.Description = strings.TrimSpace(*req.Description)
	}
	if req.Generate != nil {
		set.Spec.Generate = *req.Generate
	}
	if req.Derived != nil {
		set.Spec.Derived = *req.Derived
	}
	if !validateSetSpec(w, &set.Spec) {
		return
	}
	if req.ResourceVersion != "" {
		set.ResourceVersion = req.ResourceVersion
	}
	if err := c.Update(ctx, &set); err != nil {
		a.kubeError(w, r, p, "secret_set.update", target, secretSetNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "secret_set.update", target, fmt.Sprintf("generated %s; derived %s",
		noneOr(set.Spec.Generate), noneOr(derivedNames(set.Spec.Derived))))
	writeJSON(w, http.StatusOK, secretSetSummary(&set))
}

func derivedNames(d []kwerftv1.DerivedSecretKey) []string {
	out := make([]string, 0, len(d))
	for _, k := range d {
		out = append(out, k.Key)
	}
	return out
}

func noneOr(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// secretSetDelete removes a set; its Secret, and so every value, goes with
// it (owner reference). Apps still referencing it wait (SecretMissing).
func (a *api) secretSetDelete(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("set")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := secretTarget(project, name)
	if err := c.Delete(ctx, &kwerftv1.SecretSet{ObjectMeta: metav1.ObjectMeta{Namespace: project, Name: name}}); err != nil {
		a.kubeError(w, r, p, "secret_set.delete", target, secretSetNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "secret_set.delete", target, "")
	w.WriteHeader(http.StatusNoContent)
}

// ---- keys ------------------------------------------------------------------------

// setForWrite reads the set a key is written to, as the user. A missing set
// named <app>-env of an existing App is created as that App's own (the env
// editor's "secret" toggle). On nil it has answered.
func (a *api) setForWrite(w http.ResponseWriter, r *http.Request, ctx context.Context, c client.Client, p *principal, action string) *kwerftv1.SecretSet {
	project, name, key := r.PathValue("project"), r.PathValue("set"), r.PathValue("key")
	target := secretTarget(project, name, key)
	if !controllers.SecretKeyRE.MatchString(key) {
		invalid(w, "key", fmt.Sprintf("%q is not a valid key. Use letters, digits, -, _ and . (at most 253).", key))
		return nil
	}
	var set kwerftv1.SecretSet
	err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &set)
	if apierrors.IsNotFound(err) && strings.HasSuffix(name, controllers.AppEnvSetSuffix) && action == "secret.set" {
		app := strings.TrimSuffix(name, controllers.AppEnvSetSuffix)
		var owner kwerftv1.App
		if gerr := c.Get(ctx, types.NamespacedName{Namespace: project, Name: app}, &owner); gerr == nil {
			set = kwerftv1.SecretSet{ObjectMeta: metav1.ObjectMeta{Namespace: project, Name: name, OwnerReferences: []metav1.OwnerReference{appOwner(&owner)}}}
			if err = c.Create(ctx, &set); err == nil {
				a.audit(r, p.user.Email, "secret_set.create", secretTarget(project, name), "own set of app "+app)
			}
		}
	}
	if err != nil {
		a.kubeError(w, r, p, action, target, secretSetNotFound(project, name), err)
		return nil
	}
	if c := meta.FindStatusCondition(set.Status.Conditions, controllers.ConditionReady); c != nil && c.Reason == "Conflict" {
		writeError(w, http.StatusConflict, c.Message)
		return nil
	}
	return &set
}

// secretKeyPut sets a key to {value} or to a generated value
// ({generate: true}). The answer is the key's record, never the value.
func (a *api) secretKeyPut(w http.ResponseWriter, r *http.Request) {
	project, name, key := r.PathValue("project"), r.PathValue("set"), r.PathValue("key")
	var req struct {
		Value    *string `json:"value"`
		Generate bool    `json:"generate"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	switch {
	case req.Generate && req.Value != nil:
		invalid(w, "value", "Send a value or generate: true, not both.")
		return
	case !req.Generate && (req.Value == nil || *req.Value == ""):
		invalid(w, "value", "Enter a value, or let Kwerft generate one.")
		return
	case req.Value != nil && len(*req.Value) > maxSecretValueSize:
		invalid(w, "value", "A value can be at most 60 KiB.")
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	set := a.setForWrite(w, r, ctx, c, p, "secret.set")
	if set == nil {
		return
	}
	if derivedKey(&set.Spec, key) {
		writeError(w, http.StatusConflict, key+" is derived from other keys of the set. Change its template instead.")
		return
	}
	rec := controllers.KeyRecord{At: a.now(), By: p.user.Email, Source: controllers.SourceSet}
	var value string
	if req.Generate {
		if value, err = controllers.GenerateSecretValue(); err != nil {
			a.internalError(w, r, err)
			return
		}
		rec.Source = controllers.SourceGenerated
	} else {
		value = *req.Value
	}
	target := secretTarget(project, name, key)
	if err := a.writeKeys(ctx, c, set, map[string]any{key: []byte(value)}, map[string]any{controllers.KeyAnnotation(key): rec.Encode()}); err != nil {
		a.secretWriteError(w, r, p, "secret.set", target, err)
		return
	}
	detail := ""
	if req.Generate {
		detail = "generated"
	}
	a.audit(r, p.user.Email, "secret.set", target, detail)
	at := rec.At.UTC().Truncate(time.Second)
	writeJSON(w, http.StatusOK, secretKeyJSON{Name: key, UpdatedAt: &at, UpdatedBy: rec.By, Source: rec.Source})
}

// secretKeyDelete removes a key. Generated and derived keys would come
// back at once: change the set's lists instead.
func (a *api) secretKeyDelete(w http.ResponseWriter, r *http.Request) {
	project, name, key := r.PathValue("project"), r.PathValue("set"), r.PathValue("key")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	set := a.setForWrite(w, r, ctx, c, p, "secret.delete")
	if set == nil {
		return
	}
	switch {
	case derivedKey(&set.Spec, key):
		writeError(w, http.StatusConflict, key+" is derived from other keys of the set and would come back. Remove it from the derived keys instead.")
		return
	case slices.Contains(set.Spec.Generate, key):
		writeError(w, http.StatusConflict, key+" is generated and would come back at once. Remove it from the generated keys first, or generate a new value.")
		return
	}
	target := secretTarget(project, name, key)
	if err := a.writeKeys(ctx, c, set, map[string]any{key: nil}, map[string]any{controllers.KeyAnnotation(key): nil}); err != nil {
		a.secretWriteError(w, r, p, "secret.delete", target, err)
		return
	}
	a.audit(r, p.user.Email, "secret.delete", target, "")
	w.WriteHeader(http.StatusNoContent)
}

// writeKeys merges data (base64 by []byte, nil removes) and annotations into
// the set's Secret as the user. The patch goes to the metadata endpoint: the
// answer is the Secret's metadata, so no value comes back. A young set may
// still wait for its Secret or its place in the Role: retry briefly.
func (a *api) writeKeys(ctx context.Context, c client.Client, set *kwerftv1.SecretSet, data, annotations map[string]any) error {
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}, "data": data})
	if err != nil {
		return err
	}
	young := a.now().Sub(set.CreationTimestamp.Time) < secretSetYoung
	deadline := time.Now().Add(secretWriteWait)
	for checked := false; ; {
		obj := &metav1.PartialObjectMetadata{}
		obj.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		obj.SetNamespace(set.Namespace)
		obj.SetName(set.Name)
		err := c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
		if err == nil || !young || !(apierrors.IsNotFound(err) || apierrors.IsForbidden(err)) || time.Now().After(deadline) {
			return err
		}
		// Whoever may write the set is bound to the Role once it names the
		// Secret; anyone else (a viewer) is refused at once.
		if apierrors.IsForbidden(err) && !checked {
			checked = true
			if !mayWriteSets(ctx, c, set.Namespace) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// mayWriteSets asks Kubernetes whether the user may change secret sets in
// the namespace: exactly who the SecretSet reconciler binds to its patch
// Role there.
func mayWriteSets(ctx context.Context, c client.Client, namespace string) bool {
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: "update", Group: kwerftv1.GroupVersion.Group, Resource: "secretsets"},
	}}
	return c.Create(ctx, review) == nil && review.Status.Allowed
}

// secretWriteError answers a failed value write. Nothing of the request
// (the value) is in a Kubernetes error for a patch the console built, but
// the log keeps to the reason anyway.
func (a *api) secretWriteError(w http.ResponseWriter, r *http.Request, p *principal, action, target string, err error) {
	switch {
	case apierrors.IsForbidden(err):
		a.audit(r, p.user.Email, action+".denied", target, "forbidden by Kubernetes RBAC")
		writeError(w, http.StatusForbidden, "Your role does not allow setting secret values in this project.")
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusServiceUnavailable, "The set's Secret does not exist yet. Try again in a moment; if it persists, look at the set's status.")
	case isAPIStatus(err):
		a.cfg.Logger.Error("secret write failed", "target", target, "reason", apierrors.ReasonForError(err), "code", statusCode(err))
		writeError(w, http.StatusBadGateway, "Kubernetes refused the value ("+string(apierrors.ReasonForError(err))+").")
	default:
		a.kubeError(w, r, p, action, target, "", err)
	}
}

func statusCode(err error) int32 {
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return s.Status().Code
	}
	return 0
}

// secretKeyReveal returns one value to an owner or admin after their
// password (or a current authenticator code), audited. API tokens never
// reveal (api_tokens.go refuses the route too).
func (a *api) secretKeyReveal(w http.ResponseWriter, r *http.Request) {
	project, name, key := r.PathValue("project"), r.PathValue("set"), r.PathValue("key")
	p := principalOf(r)
	target := secretTarget(project, name, key)
	if p.token != nil {
		writeError(w, http.StatusForbidden, "API tokens never reveal secret values. Use the console.")
		return
	}
	if !access.Allowed(p.user.Role, access.RevealSecrets) {
		a.audit(r, p.user.Email, "secret.reveal.denied", target, "role "+p.user.Role)
		writeError(w, http.StatusForbidden, "Only owners and admins reveal secret values.")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) || !a.confirmIdentity(w, r, p.user, req.Password) {
		return
	}
	c, _, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &sec); err != nil {
		a.kubeError(w, r, p, "secret.reveal", target, secretSetNotFound(project, name), err)
		return
	}
	value, ok := sec.Data[key]
	if sec.Labels[controllers.LabelSecretSet] != name || !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Secret set %q has no key %s.", name, key))
		return
	}
	a.audit(r, p.user.Email, "secret.reveal", target, "")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": string(value)})
}

// secretSetCopy copies a set with its values into another project (owners
// and admins): a new set there, no live link. Read and written as the user,
// like reveal and setting keys.
func (a *api) secretSetCopy(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("set")
	var req struct {
		Project string `json:"project"`
		Name    string `json:"name"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	p := principalOf(r)
	if !access.Allowed(p.user.Role, access.CopySecretSets) {
		writeError(w, http.StatusForbidden, "Only owners and admins copy secret sets between projects.")
		return
	}
	req.Project, req.Name = strings.TrimSpace(req.Project), strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = name
	}
	switch {
	case req.Project == "":
		invalid(w, "project", "Choose the project to copy to.")
		return
	case req.Project == project && req.Name == name:
		invalid(w, "name", "Give the copy another name, or choose another project.")
		return
	case !validSetName(req.Name):
		invalid(w, "name", "Use lowercase letters, digits, - and ., starting and ending with a letter or digit (at most 63).")
		return
	case p.token != nil && p.token.Projects != nil && !slices.Contains(p.token.Projects, req.Project):
		writeError(w, http.StatusForbidden, "This token is limited to the projects "+strings.Join(p.token.Projects, ", ")+".")
		return
	}
	src, _, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	from := secretTarget(project, name)
	var set kwerftv1.SecretSet
	if err := src.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &set); err != nil {
		a.kubeError(w, r, p, "secret_set.copy", from, secretSetNotFound(project, name), err)
		return
	}
	var sec corev1.Secret
	if err := src.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &sec); err != nil {
		a.kubeError(w, r, p, "secret_set.copy", from, secretSetNotFound(project, name), err)
		return
	}
	if sec.Labels[controllers.LabelSecretSet] != name {
		writeError(w, http.StatusConflict, "This set does not manage its Secret (see its status); there is nothing to copy.")
		return
	}

	conn, err := a.clusters.forProject(ctx, req.Project)
	if err != nil {
		clusterError(w, err)
		return
	}
	dst, err := conn.kube.For(p.user.Email, p.user.Role)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	dctx := withCluster(ctx, conn, true)
	to := secretTarget(req.Project, req.Name)
	copied := &kwerftv1.SecretSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: req.Project, Name: req.Name},
		Spec:       kwerftv1.SecretSetSpec{Description: set.Spec.Description, Generate: set.Spec.Generate, Derived: set.Spec.Derived},
	}
	if err := dst.Create(dctx, copied); err != nil {
		a.kubeError(w, r.WithContext(dctx), p, "secret_set.copy", to, fmt.Sprintf("Project %q not found.", req.Project), err)
		return
	}
	data := map[string]any{}
	ann := map[string]any{}
	now := a.now()
	for _, k := range slices.Sorted(maps.Keys(sec.Data)) {
		data[k] = sec.Data[k]
		src := controllers.SourceSet
		if derivedKey(&set.Spec, k) {
			src = controllers.SourceDerived
		} else if slices.Contains(set.Spec.Generate, k) {
			src = controllers.SourceGenerated
		}
		ann[controllers.KeyAnnotation(k)] = controllers.KeyRecord{At: now, By: p.user.Email, Source: src}.Encode()
	}
	if len(data) > 0 {
		if err := a.writeKeys(dctx, dst, copied, data, ann); err != nil {
			// No half copy: the set goes again.
			if derr := dst.Delete(dctx, copied); client.IgnoreNotFound(derr) != nil {
				a.cfg.Logger.Error("could not remove a failed copy", "set", to, "err", derr)
			}
			a.secretWriteError(w, r.WithContext(dctx), p, "secret_set.copy", to, err)
			return
		}
	}
	a.audit(r, p.user.Email, "secret_set.copy", from, fmt.Sprintf("to %s, %s", to, keysCount(len(data))))
	writeJSON(w, http.StatusCreated, secretSetSummary(copied))
}

func keysCount(n int) string {
	if n == 1 {
		return "1 key"
	}
	return fmt.Sprintf("%d keys", n)
}
