// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// BackupPlans: each becomes the Velero Schedule kwerft-<plan> in the
// namespace velero (controlled by the plan, so deleting the plan deletes the
// Schedule; its backups stay until they expire). Every Backup made from it
// carries kwerft.dev/backup-plan and kwerft.dev/backup-scope. A Cluster plan
// names every project's namespace (and Kwerft's own) explicitly, so the
// Schedule follows projects as they come and go. "Back up now" is the
// annotation kwerft.dev/run-requested: the reconciler makes a Backup from
// the plan's template. The status comes from the plan's Velero Backups.

const backupPlanResync = 10 * time.Minute

// BackupPlanReconciler: see above.
type BackupPlanReconciler struct {
	client.Client
	Now func() time.Time
	// watching: Velero's kinds are watched (installed when the manager
	// started); otherwise the reconciler polls.
	watching bool
}

func (r *BackupPlanReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *BackupPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var plan kwerftv1.BackupPlan
	if err := r.Get(ctx, req.NamespacedName, &plan); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !plan.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := plan.DeepCopy()
	plan.Status.ObservedGeneration = plan.Generation
	resync := backupPlanResync
	if !r.watching {
		resync = time.Minute
	}

	sched, err := ParseBackupSchedule(plan.Spec.Schedule)
	if err != nil {
		plan.Status.NextRunAt = nil
		setReady(&plan.Status.Conditions, plan.Generation, metav1.ConditionFalse, "InvalidSchedule",
			"The schedule is not a cron expression with five fields: "+err.Error())
		return ctrl.Result{}, r.writeStatus(ctx, &plan, orig)
	}

	template, err := r.template(ctx, &plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	installed, err := r.applySchedule(ctx, &plan, template)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !installed {
		plan.Status.NextRunAt = nil
		setReady(&plan.Status.Conditions, plan.Generation, metav1.ConditionFalse, "NoVelero",
			"Velero is not installed: re-run the installer (stage Backups; --lite leaves it out).")
		return ctrl.Result{RequeueAfter: time.Minute}, r.writeStatus(ctx, &plan, orig)
	}
	if started, err := r.runRequested(ctx, &plan, template); err != nil || started {
		// Marking the request handled changed the plan: the next pass,
		// which that change starts, writes the status.
		return ctrl.Result{}, err
	}
	if err := r.summarize(ctx, &plan); err != nil {
		return ctrl.Result{}, err
	}

	now := r.now()
	if plan.Spec.Paused {
		plan.Status.NextRunAt = nil
	} else {
		next := sched.Next(now.UTC())
		plan.Status.NextRunAt = &metav1.Time{Time: next}
		if until := next.Sub(now) + time.Minute; until < resync {
			resync = until
		}
	}
	schedule := newVelero(VeleroScheduleGVK)
	err = r.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: BackupScheduleName(plan.Name)}, schedule)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	status, reason, message := r.readiness(ctx, &plan, schedule)
	setReady(&plan.Status.Conditions, plan.Generation, status, reason, message)
	return ctrl.Result{RequeueAfter: resync}, r.writeStatus(ctx, &plan, orig)
}

// readiness says whether the plan's backups can run.
func (r *BackupPlanReconciler) readiness(ctx context.Context, plan *kwerftv1.BackupPlan, schedule *unstructured.Unstructured) (metav1.ConditionStatus, string, string) {
	if nestedString(schedule, "status", "phase") == "FailedValidation" {
		errs, _, _ := unstructured.NestedStringSlice(schedule.Object, "status", "validationErrors")
		return metav1.ConditionFalse, "InvalidSchedule", "Velero rejected the schedule: " + strings.Join(errs, "; ")
	}
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err == nil && s.Status.Backups != nil {
		switch st := s.Status.Backups; st.State {
		case "Ready":
		case "Pending":
			return metav1.ConditionFalse, "TargetPending", "Waiting for Velero to check the bucket."
		default:
			return metav1.ConditionFalse, "TargetNotReady", st.Message
		}
	} else {
		return metav1.ConditionFalse, "TargetNotReady", "No backup target is set. Enter one under Settings › Backups."
	}
	if plan.Spec.Paused {
		return metav1.ConditionTrue, "Paused", "Paused: scheduled runs are skipped; Back up now still works."
	}
	return metav1.ConditionTrue, "Scheduled", "Backs up on schedule."
}

func (r *BackupPlanReconciler) writeStatus(ctx context.Context, plan, orig *kwerftv1.BackupPlan) error {
	if equality.Semantic.DeepEqual(orig.Status, plan.Status) {
		return nil
	}
	if err := patchStatus(ctx, r.Client, plan, orig); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// template is the plan's Velero BackupSpec with the current projects.
func (r *BackupPlanReconciler) template(ctx context.Context, plan *kwerftv1.BackupPlan) (map[string]any, error) {
	var names []string
	if PlanScope(plan) == kwerftv1.BackupCluster {
		var projects kwerftv1.ProjectList
		if err := r.List(ctx, &projects); err != nil {
			return nil, err
		}
		for _, p := range projects.Items {
			if p.DeletionTimestamp.IsZero() {
				names = append(names, p.Name)
			}
		}
	}
	return backupTemplate(plan, names), nil
}

// applySchedule writes the plan's Schedule; false when Velero is missing.
func (r *BackupPlanReconciler) applySchedule(ctx context.Context, plan *kwerftv1.BackupPlan, template map[string]any) (bool, error) {
	u := newVelero(VeleroScheduleGVK)
	u.SetName(BackupScheduleName(plan.Name))
	u.SetNamespace(VeleroNamespace)
	labels := backupLabels(plan)
	labels[LabelManagedBy] = ManagedByKwerft
	u.SetLabels(labels)
	gvk := kwerftv1.GroupVersion.WithKind("BackupPlan")
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: plan.Name, UID: plan.UID,
		Controller: ptrTo(true), BlockOwnerDeletion: ptrTo(true),
	}})
	u.Object["spec"] = map[string]any{
		"schedule": strings.TrimSpace(plan.Spec.Schedule),
		"paused":   plan.Spec.Paused,
		// The first backup comes on schedule (or from "Back up now"), not
		// the moment the plan is saved.
		"skipImmediately":            true,
		"useOwnerReferencesInBackup": false,
		"template":                   template,
	}
	err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership)
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// runRequested makes the Backup "Back up now" asked for, once per request.
func (r *BackupPlanReconciler) runRequested(ctx context.Context, plan *kwerftv1.BackupPlan, template map[string]any) (bool, error) {
	requested := plan.Annotations[AnnotationRunRequested]
	if requested == "" || requested == plan.Annotations[annotationRunHandled] {
		return false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, requested)
	if err != nil {
		at = r.now()
	}
	b := newVelero(VeleroBackupGVK)
	b.SetName(BackupScheduleName(plan.Name) + "-" + at.UTC().Format("20060102150405"))
	b.SetNamespace(VeleroNamespace)
	labels := backupLabels(plan)
	labels[labelVeleroScheduleName] = BackupScheduleName(plan.Name)
	b.SetLabels(labels)
	ann := map[string]string{AnnotationRunRequested: requested}
	if by := plan.Annotations[AnnotationRequestedBy]; by != "" {
		ann[AnnotationRequestedBy] = by
	}
	b.SetAnnotations(ann)
	spec := map[string]any{}
	for k, v := range template {
		if k != "metadata" {
			spec[k] = v
		}
	}
	b.Object["spec"] = spec
	if err := r.Create(ctx, b); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	log.FromContext(ctx).Info("started a backup", "plan", plan.Name, "backup", b.GetName())
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": plan.ResourceVersion,
		"annotations":     map[string]string{annotationRunHandled: requested},
	}})
	return true, r.Patch(ctx, plan.DeepCopy(), client.RawPatch(types.MergePatchType, patch))
}

// summarize fills the status from the plan's Velero Backups.
func (r *BackupPlanReconciler) summarize(ctx context.Context, plan *kwerftv1.BackupPlan) error {
	list := newVeleroList(VeleroBackupGVK)
	if err := r.List(ctx, list, client.InNamespace(VeleroNamespace), client.MatchingLabels{LabelBackupPlan: plan.Name}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	runs := make([]VeleroBackup, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].GetDeletionTimestamp() != nil {
			continue
		}
		runs = append(runs, ReadVeleroBackup(&list.Items[i]))
	}
	slices.SortFunc(runs, func(a, b VeleroBackup) int { return b.Started().Compare(a.Started()) })
	st := &plan.Status
	st.Backups = 0
	st.LastBackup = nil
	var lastOK *metav1.Time
	for _, b := range runs {
		if b.Phase == "Deleting" {
			continue
		}
		st.Backups++
		if st.LastBackup == nil {
			st.LastBackup = backupRun(b)
		}
		if b.Phase == "Completed" && b.CompletedAt != nil && (lastOK == nil || b.CompletedAt.After(lastOK.Time)) {
			lastOK = &metav1.Time{Time: *b.CompletedAt}
		}
	}
	// Backups expire; the newest success stays known.
	if lastOK != nil && (st.LastSuccessfulAt == nil || lastOK.After(st.LastSuccessfulAt.Time)) {
		st.LastSuccessfulAt = lastOK
	}
	return nil
}

func backupRun(b VeleroBackup) *kwerftv1.BackupRun {
	t := func(p *time.Time) *metav1.Time {
		if p == nil {
			return nil
		}
		return &metav1.Time{Time: *p}
	}
	return &kwerftv1.BackupRun{
		Name: b.Name, Phase: b.Phase, StartedAt: t(b.StartedAt), CompletedAt: t(b.CompletedAt), ExpiresAt: t(b.ExpiresAt),
		Items: int32(min(b.Items, 1<<31-1)), Warnings: int32(min(b.Warnings, 1<<31-1)), Errors: int32(min(b.Errors, 1<<31-1)),
		Message: b.Message,
	}
}

func (r *BackupPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allPlans := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var plans kwerftv1.BackupPlanList
		if err := mgr.GetClient().List(ctx, &plans); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(plans.Items))
		for _, p := range plans.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
		}
		return out
	})
	byLabel := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if plan := o.GetLabels()[LabelBackupPlan]; plan != "" && o.GetNamespace() == VeleroNamespace {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: plan}}}
		}
		return nil
	})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("backupplan").
		// Spec changes and "Back up now" (an annotation); not its status.
		For(&kwerftv1.BackupPlan{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		// A Cluster plan backs up every project.
		Watches(&kwerftv1.Project{}, allPlans, builder.WithPredicates(predicate.Funcs{UpdateFunc: func(event.UpdateEvent) bool { return false }})).
		// The target's state is the plans' readiness.
		Watches(&kwerftv1.ConsoleSettings{}, allPlans, builder.WithPredicates(predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
			o, n := e.ObjectOld.(*kwerftv1.ConsoleSettings).Status.Backups, e.ObjectNew.(*kwerftv1.ConsoleSettings).Status.Backups
			return (o == nil) != (n == nil) || o != nil && (o.State != n.State || o.Message != n.Message)
		}}))
	if veleroInstalled(mgr, VeleroBackupGVK) && veleroInstalled(mgr, VeleroScheduleGVK) {
		r.watching = true
		b = b.Watches(newVelero(VeleroBackupGVK), byLabel, builder.WithPredicates(veleroPhaseChanged())).
			Watches(newVelero(VeleroScheduleGVK), byLabel, builder.WithPredicates(veleroPhaseChanged()))
	}
	return b.Complete(r)
}
