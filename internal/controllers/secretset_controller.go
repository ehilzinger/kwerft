package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// SecretSetReconciler keeps a project's secret sets (docs/plan.md ›
// Secrets):
//   - the Secret of the set's name, labelled LabelSecretSet and owned by the
//     set, created empty. A Secret of that name it did not create is never
//     taken over (Ready False, reason Conflict);
//   - spec.generate keys, filled with GenerateSecretValue while missing, and
//     spec.derived keys, rendered from their ${KEY} templates whenever an
//     input changes;
//   - the status: key names with their KeyRecord (never values), who uses
//     the set and which referenced keys it lacks;
//   - per namespace, the Roles SecretSetsRole (patch) and SecretSetsReadRole
//     (get) on exactly the managed Secrets, and their RoleBindings: patch for
//     owners, admins and the project's developers (ProjectBindings), get for
//     owners and admins. resourceNames is never empty (that would be every
//     Secret): without sets the Roles have no rules.
//
// Secrets are read uncached (APIReader); their metadata is watched, so a
// value the console writes updates the status (and the App reconciler rolls
// the Apps using it).
type SecretSetReconciler struct {
	client.Client
	// APIReader reads Secrets; nil falls back to the client (tests).
	APIReader client.Reader
	Now       func() time.Time
}

func (r *SecretSetReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *SecretSetReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *SecretSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var set kwerftv1.SecretSet
	err := r.Get(ctx, req.NamespacedName, &set)
	if apierrors.IsNotFound(err) || err == nil && !set.DeletionTimestamp.IsZero() {
		// Its Secret goes with it (owner reference); the Roles stop naming it.
		return ctrl.Result{}, r.syncRoles(ctx, req.Namespace)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	orig := set.DeepCopy()

	ready, err := r.reconcile(ctx, &set)
	if apierrors.IsConflict(err) {
		return ctrl.Result{}, err // the Secret changed meanwhile: start over, status untouched
	}
	if err == nil {
		// After the Secret exists (or not, on a conflict), so the Roles name
		// only Secrets this set manages.
		err = r.syncRoles(ctx, set.Namespace)
	}
	switch {
	case err != nil:
		setReady(&set.Status.Conditions, set.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
	default:
		setReady(&set.Status.Conditions, set.Generation, ready.status, ready.reason, ready.message)
	}
	set.Status.ObservedGeneration = set.Generation
	if !equality.Semantic.DeepEqual(orig.Status, set.Status) {
		if perr := patchStatus(ctx, r.Client, &set, orig); perr != nil {
			return ctrl.Result{}, client.IgnoreNotFound(perr)
		}
	}
	if isTerminal(err) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, err
}

func (r *SecretSetReconciler) reconcile(ctx context.Context, set *kwerftv1.SecretSet) (*readiness, error) {
	if _, err := projectOf(ctx, r.Client, set.Namespace); err != nil {
		return nil, err
	}
	sec, err := r.ensureSecret(ctx, set)
	if err != nil {
		set.Status.Keys = nil
		return nil, err
	}
	pending, err := r.fill(ctx, set, sec)
	if err != nil {
		return nil, err
	}

	set.Status.Keys = keyStatus(sec)
	users, missing, err := secretSetUsers(ctx, r.Client, set.Namespace, set.Name, sec.Data)
	if err != nil {
		return nil, err
	}
	set.Status.UsedBy, set.Status.Missing = users, missing

	switch {
	case len(pending) > 0:
		return &readiness{metav1.ConditionFalse, "DerivedPending", strings.Join(pending, "; ")}, nil
	case len(missing) > 0:
		return &readiness{metav1.ConditionFalse, "MissingKeys", "Referenced but not set: " + strings.Join(missing, ", ")}, nil
	}
	return &readiness{metav1.ConditionTrue, "Ready", plural(len(sec.Data), "1 key", fmt.Sprintf("%d keys", len(sec.Data)))}, nil
}

// ensureSecret returns the set's Secret, creating it empty when missing.
func (r *SecretSetReconciler) ensureSecret(ctx context.Context, set *kwerftv1.SecretSet) (*corev1.Secret, error) {
	key := client.ObjectKeyFromObject(set)
	var sec corev1.Secret
	err := r.reader().Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: set.Name, Namespace: set.Namespace,
				Labels: map[string]string{LabelManagedBy: ManagedByKwerft, LabelSecretSet: set.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kwerftv1.GroupVersion.String(), Kind: "SecretSet", Name: set.Name, UID: set.UID,
					Controller: ptrTo(true), BlockOwnerDeletion: ptrTo(true),
				}},
			},
			Type: corev1.SecretTypeOpaque,
		}
		if err := r.Create(ctx, &sec); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("secret %s appeared meanwhile; retrying", key.Name)
			}
			return nil, err
		}
		log.FromContext(ctx).Info("created secret set secret", "secret", key)
		return &sec, nil
	}
	if err != nil {
		return nil, err
	}
	if metav1.IsControlledBy(&sec, set) && sec.Labels[LabelSecretSet] == set.Name {
		return &sec, nil
	}
	owner := metav1.GetControllerOf(&sec)
	if owner != nil && owner.Kind == "SecretSet" && owner.Name == set.Name && sec.Labels[LabelSecretSet] == set.Name {
		// Left by an earlier set of the same name that is gone: its values
		// were that set's, and go with it as garbage collection would.
		if err := r.Delete(ctx, &sec, client.Preconditions{UID: &sec.UID}); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
		return nil, fmt.Errorf("removed the Secret %s left by an earlier set; recreating it", key.Name)
	}
	return nil, terminalf("Conflict",
		"A Secret named %s exists in this project and is not managed by this set; Kwerft never takes it over. Pick another name for the set, or remove that Secret.", set.Name)
}

// fill writes missing generated keys and out-of-date derived keys into sec
// (patching the Secret). pending explains derived keys that wait for input.
func (r *SecretSetReconciler) fill(ctx context.Context, set *kwerftv1.SecretSet, sec *corev1.Secret) (pending []string, err error) {
	values := maps.Clone(sec.Data)
	if values == nil {
		values = map[string][]byte{}
	}
	changed := map[string]KeyRecord{}
	at := r.now()
	for _, k := range set.Spec.Generate {
		if _, ok := values[k]; ok {
			continue
		}
		v, err := GenerateSecretValue()
		if err != nil {
			return nil, err
		}
		values[k] = []byte(v)
		changed[k] = KeyRecord{At: at, Source: SourceGenerated}
	}
	// Derived keys may build on each other: render until nothing changes.
	waiting := map[string][]string{}
	for range len(set.Spec.Derived) + 1 {
		progress := false
		for _, d := range set.Spec.Derived {
			if slices.Contains(TemplateKeys(d.Template), d.Key) {
				waiting[d.Key] = []string{d.Key}
				continue
			}
			out, missing := renderTemplate(d.Template, values)
			if len(missing) > 0 {
				waiting[d.Key] = missing
				continue
			}
			delete(waiting, d.Key)
			if cur, ok := values[d.Key]; ok && string(cur) == out {
				continue
			}
			values[d.Key] = []byte(out)
			changed[d.Key] = KeyRecord{At: at, Source: SourceDerived}
			progress = true
		}
		if !progress {
			break
		}
	}
	for _, d := range set.Spec.Derived {
		if keys, ok := waiting[d.Key]; ok {
			if slices.Contains(keys, d.Key) {
				pending = append(pending, d.Key+" refers to itself")
				continue
			}
			pending = append(pending, d.Key+" waits for "+strings.Join(keys, ", "))
		}
	}
	if len(changed) == 0 {
		return pending, nil
	}
	data := map[string][]byte{}
	ann := map[string]string{}
	for k, rec := range changed {
		data[k] = values[k]
		ann[KeyAnnotation(k)] = rec.Encode()
	}
	// Guarded by the version read: a value the console wrote meanwhile is
	// not overwritten with one derived from the old inputs (a conflict
	// retries with fresh data).
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann, "resourceVersion": sec.ResourceVersion}, "data": data})
	if err != nil {
		return nil, err
	}
	target := sec.DeepCopy()
	if err := r.Patch(ctx, target, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return nil, err
	}
	keys := slices.Sorted(maps.Keys(changed))
	log.FromContext(ctx).Info("filled secret set keys", "set", set.Name, "keys", keys)
	*sec = *target
	return pending, nil
}

// keyStatus lists a Secret's keys with their records, sorted by name.
func keyStatus(sec *corev1.Secret) []kwerftv1.SecretKeyStatus {
	out := make([]kwerftv1.SecretKeyStatus, 0, len(sec.Data))
	for _, k := range slices.Sorted(maps.Keys(sec.Data)) {
		ks := kwerftv1.SecretKeyStatus{Name: k}
		if rec, ok := keyRecord(sec.Annotations, k); ok {
			t := metav1.NewTime(rec.At)
			ks.UpdatedAt, ks.UpdatedBy, ks.Source = &t, rec.By, rec.Source
		}
		out = append(out, ks)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// secretSetUsers lists who references the set (env or mounted as files) as
// "Kind/name", and the env references to keys it does not have as
// "Kind/name: KEY": Apps, Schedules, and Tasks that have not finished
// (a Task a Schedule started counts as the Schedule). A Task or Schedule
// from an App counts when the App uses the set.
func secretSetUsers(ctx context.Context, c client.Reader, namespace, set string, data map[string][]byte) (users, missing []string, err error) {
	check := func(who string, env []corev1.EnvVar, vols []kwerftv1.AppVolume) bool {
		uses := slices.ContainsFunc(vols, func(v kwerftv1.AppVolume) bool { return v.Secret == set })
		for _, ref := range envSecretRefs(env) {
			if ref.Name != set {
				continue
			}
			uses = true
			if _, ok := data[ref.Key]; !ok && (ref.Optional == nil || !*ref.Optional) {
				m := who + ": " + ref.Key
				if !slices.Contains(missing, m) {
					missing = append(missing, m)
				}
			}
		}
		return uses
	}

	var apps kwerftv1.AppList
	if err := c.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		return nil, nil, err
	}
	appUses := map[string]bool{}
	for _, a := range apps.Items {
		if !a.DeletionTimestamp.IsZero() {
			continue
		}
		if check("App/"+a.Name, a.Spec.Env, a.Spec.Volumes) {
			appUses[a.Name] = true
			users = append(users, "App/"+a.Name)
		}
	}
	var schedules kwerftv1.ScheduleList
	if err := c.List(ctx, &schedules, client.InNamespace(namespace)); err != nil {
		return nil, nil, err
	}
	for _, s := range schedules.Items {
		if !s.DeletionTimestamp.IsZero() {
			continue
		}
		who := "Schedule/" + s.Name
		if check(who, s.Spec.Task.Env, s.Spec.Task.Volumes) || appUses[s.Spec.Task.FromApp] {
			users = append(users, who)
		}
	}
	var tasks kwerftv1.TaskList
	if err := c.List(ctx, &tasks, client.InNamespace(namespace)); err != nil {
		return nil, nil, err
	}
	for _, t := range tasks.Items {
		if !t.DeletionTimestamp.IsZero() || t.Status.Phase.Finished() || t.Labels[LabelSchedule] != "" {
			continue
		}
		who := "Task/" + t.Name
		if check(who, slices.Concat(t.Spec.Env, t.Spec.EnvOverrides), t.Spec.Volumes) || appUses[t.Spec.FromApp] {
			users = append(users, who)
		}
	}
	slices.Sort(users)
	slices.Sort(missing)
	return users, missing, nil
}

// syncRoles keeps the namespace's Roles naming exactly the Secrets that a
// SecretSet there manages, and their bindings to who may write and read
// them.
func (r *SecretSetReconciler) syncRoles(ctx context.Context, namespace string) error {
	var sets kwerftv1.SecretSetList
	if err := r.List(ctx, &sets, client.InNamespace(namespace)); err != nil {
		return err
	}
	live := map[string]types.UID{}
	for _, s := range sets.Items {
		if s.DeletionTimestamp.IsZero() {
			live[s.Name] = s.UID
		}
	}
	var secrets metav1.PartialObjectMetadataList
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.reader().List(ctx, &secrets, client.InNamespace(namespace), client.HasLabels{LabelSecretSet}); err != nil {
		return err
	}
	var names []string
	for _, s := range secrets.Items {
		owner := metav1.GetControllerOf(&s)
		uid, ok := live[s.Name]
		if ok && s.Labels[LabelSecretSet] == s.Name && owner != nil && owner.Kind == "SecretSet" && owner.UID == uid && s.DeletionTimestamp.IsZero() {
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)

	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return client.IgnoreNotFound(err)
	}
	if ns.Labels[LabelProject] == "" || !ns.DeletionTimestamp.IsZero() {
		return nil
	}
	writers := []rbacv1.Subject{groupSubject("owner"), groupSubject("admin")}
	var project kwerftv1.Project
	switch err := r.Get(ctx, client.ObjectKey{Name: ns.Labels[LabelProject]}, &project); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	default:
		for _, b := range ProjectBindings(&project) {
			if b.ClusterRole == ProjectDeveloperRole {
				writers = append(writers, b.Subjects...)
			}
		}
	}
	readers := []rbacv1.Subject{groupSubject("owner"), groupSubject("admin")}

	for _, role := range []struct {
		name     string
		verb     string
		subjects []rbacv1.Subject
	}{{SecretSetsRole, "patch", writers}, {SecretSetsReadRole, "get", readers}} {
		ac := rbacv1ac.Role(role.name, namespace).WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft})
		if len(names) > 0 {
			ac.WithRules(rbacv1ac.PolicyRule().WithAPIGroups("").WithResources("secrets").WithVerbs(role.verb).WithResourceNames(names...))
		}
		if err := apply(ctx, r.Client, ac); err != nil {
			return fmt.Errorf("apply role %s: %w", role.name, err)
		}
		rb := rbacv1ac.RoleBinding(role.name, namespace).
			WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
			WithRoleRef(rbacv1ac.RoleRef().WithAPIGroup(rbacv1.GroupName).WithKind("Role").WithName(role.name))
		for _, s := range role.subjects {
			rb.WithSubjects(rbacv1ac.Subject().WithAPIGroup(s.APIGroup).WithKind(s.Kind).WithName(s.Name))
		}
		if err := apply(ctx, r.Client, rb); err != nil {
			return fmt.Errorf("apply role binding %s: %w", role.name, err)
		}
	}
	return nil
}

// setsInNamespace enqueues every SecretSet in the object's namespace (an
// App, Task or Schedule may have started or stopped using one).
func (r *SecretSetReconciler) setsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.setsIn(ctx, obj.GetNamespace())
}

// setsInProject enqueues a Project's sets: its members decide the bindings.
func (r *SecretSetReconciler) setsInProject(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.setsIn(ctx, obj.GetName())
}

func (r *SecretSetReconciler) setsIn(ctx context.Context, namespace string) []reconcile.Request {
	var list kwerftv1.SecretSetList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, s := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
	}
	return out
}

// setOfSecret enqueues the set a labelled Secret belongs to.
func setOfSecret(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[LabelSecretSet]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

func (r *SecretSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inNamespace := handler.EnqueueRequestsFromMapFunc(r.setsInNamespace)
	specChanged := builder.WithPredicates(predicate.GenerationChangedPredicate{})
	return ctrl.NewControllerManagedBy(mgr).
		Named("secretset").
		For(&kwerftv1.SecretSet{}, specChanged).
		// Values change without the set changing: the console patches the
		// Secret. Metadata only: values never sit in the cache.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(setOfSecret), builder.OnlyMetadata,
			builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetLabels()[LabelSecretSet] != "" }))).
		Watches(&kwerftv1.App{}, inNamespace, specChanged).
		Watches(&kwerftv1.Schedule{}, inNamespace, specChanged).
		// Tasks also stop counting when they finish (a status change).
		Watches(&kwerftv1.Task{}, inNamespace).
		Watches(&kwerftv1.Project{}, handler.EnqueueRequestsFromMapFunc(r.setsInProject), specChanged).
		Complete(r)
}
