package controllers

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// UpgradeReconciler runs Upgrades (docs/phase6-upgrades.md): one at a time
// per cluster, oldest first, the others Queued. For a Kwerft upgrade it
// runs the preflight, copies the database and starts the runner Job, which
// takes over from Running on; it watches the Job and records a runner that
// gave up. Finished Upgrades beyond the newest KeepUpgrades are deleted
// with their log ConfigMaps. Kubernetes upgrades are handed to Kubernetes
// (U4) once they are the active one.
type UpgradeReconciler struct {
	client.Client
	// APIReader lists Upgrades uncached, so two are never started at once
	// from a stale cache.
	APIReader client.Reader
	// Namespace of the runner Job and the log ConfigMaps (kwerft-system).
	Namespace string
	// Checks is the Kwerft preflight.
	Checks *UpgradeChecks
	// Database is the console's store (in this process, which owns it); nil
	// in agent mode, which has no database.
	Database upgrades.Snapshotter
	// DataDir is the console's data directory (the copy goes to
	// <DataDir>/backups).
	DataDir string
	// RunnerImage returns the running console image pinned by digest.
	RunnerImage func(ctx context.Context) (string, error)
	// KubernetesRunnerImage is the runner's image for a Kubernetes
	// upgrade's etcd snapshot; nil means RunnerImage. Unlike a Kwerft
	// upgrade, which a development install refuses, it may fall back to
	// the image as the Deployment names it (an imported dev image).
	KubernetesRunnerImage func(ctx context.Context) (string, error)
	// InstallBaseURL is passed to the runner (empty: the public install
	// repository).
	InstallBaseURL string
	// Faults passes AnnotationFault on to runners (chart value e2e.faults).
	Faults bool
	// Kubernetes drives Kubernetes upgrades (U4); nil fails them.
	Kubernetes KubernetesUpgrades
	// Keep is how many Upgrades are kept (default KeepUpgrades).
	Keep int
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// KubernetesUpgrades drives the active Kubernetes Upgrade from Preflight to
// a finished phase (docs/phase6-upgrades.md › Kubernetes upgrade). It
// changes u.Status; the Upgrade controller writes it (merge patch with an
// optimistic lock). The queue, cancellation before Preflight, AutoPatch's
// pause and the retention stay the Upgrade controller's.
type KubernetesUpgrades interface {
	Reconcile(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error)
}

const (
	// KeepUpgrades: the newest Upgrades kept; older finished ones go.
	KeepUpgrades = 20
	// upgradeFinalizer holds an Upgrade while its runner may run, so
	// deleting it cannot pull the runner from under an installer.
	upgradeFinalizer = "kwerft.dev/upgrade"
	// ConditionAutoPatchPaused on an auto-update that paused AutoPatch.
	ConditionAutoPatchPaused = "AutoPatchPaused"
)

func (r *UpgradeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *UpgradeReconciler) ns() string { return cmp.Or(r.Namespace, GatewayNamespace) }

func (r *UpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kwerftv1.Upgrade{}).
		Owns(&batchv1.Job{}).
		// The queue moves when any Upgrade changes.
		Watches(&kwerftv1.Upgrade{}, handler.EnqueueRequestsFromMapFunc(r.waiting)).
		Named("upgrade").
		Complete(r)
}

// waiting enqueues every Upgrade that waits for its turn.
func (r *UpgradeReconciler) waiting(ctx context.Context, _ client.Object) []reconcile.Request {
	var list kwerftv1.UpgradeList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, u := range list.Items {
		if u.Status.Phase == "" || u.Status.Phase == kwerftv1.UpgradeQueued {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&u)})
		}
	}
	return out
}

func (r *UpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var u kwerftv1.Upgrade
	if err := r.Get(ctx, req.NamespacedName, &u); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The finalizer holds an Upgrade while its runner may run: a deleted
	// one goes once the runner is done (finished), a running one keeps
	// going.
	holds := holdsRunner(&u)
	switch has := controllerutil.ContainsFinalizer(&u, upgradeFinalizer); {
	case has && !holds && !u.DeletionTimestamp.IsZero():
		controllerutil.RemoveFinalizer(&u, upgradeFinalizer)
		return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, &u))
	case !has && holds && u.DeletionTimestamp.IsZero():
		controllerutil.AddFinalizer(&u, upgradeFinalizer)
		if err := r.Update(ctx, &u); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	orig := u.DeepCopy()
	res, err := r.reconcile(ctx, &u)
	u.Status.ObservedGeneration = u.Generation
	if !equality.Semantic.DeepEqual(orig.Status, u.Status) {
		if perr := patchStatus(ctx, r.Client, &u, orig); perr != nil {
			if apierrors.IsConflict(perr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, perr
		}
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if upgrades.Finished(u.Status.Phase) {
		if err := r.finished(ctx, &u); apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		} else if err != nil {
			return ctrl.Result{}, err
		}
	}
	return res, nil
}

func (r *UpgradeReconciler) reconcile(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	switch p := u.Status.Phase; {
	case p == "" || p == kwerftv1.UpgradeQueued:
		return r.queue(ctx, u)
	case upgrades.Finished(p):
		return ctrl.Result{}, nil
	case u.Spec.Component == kwerftv1.UpgradeKubernetes:
		if r.Kubernetes == nil {
			r.fail(u, "Preflight", "Kubernetes upgrades are not available in this release.")
			return ctrl.Result{}, nil
		}
		if p == kwerftv1.UpgradePreflight {
			if res, stop := r.beforePreflight(ctx, u); stop {
				return res, nil
			}
		}
		return r.Kubernetes.Reconcile(ctx, u)
	case p == kwerftv1.UpgradePreflight:
		return r.preflight(ctx, u)
	case p == kwerftv1.UpgradeBackingUp:
		return r.backup(ctx, u)
	default:
		return r.observeRunner(ctx, u)
	}
}

// queue starts u when it is its turn: no Upgrade active, none older
// waiting, and (auto-updates) inside the maintenance window.
func (r *UpgradeReconciler) queue(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	if by := u.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
		r.cancel(u, by)
		return ctrl.Result{}, nil
	}
	// Held (an agent cluster's turn in an "Upgrade all"): the console
	// removes the annotation when it is this one's turn.
	if hold := u.Annotations[kwerftv1.AnnotationHold]; hold != "" {
		r.wait(u, hold)
		return ctrl.Result{}, nil
	}
	var list kwerftv1.UpgradeList
	if err := r.APIReader.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}
	for _, o := range list.Items {
		if o.Name == u.Name {
			continue
		}
		if upgrades.Active(o.Status.Phase) {
			r.wait(u, "Waiting for "+o.Name+" to finish.")
			return ctrl.Result{}, nil
		}
		// A held one is not in line yet: it does not hold the others.
		if !upgrades.Finished(o.Status.Phase) && o.DeletionTimestamp.IsZero() && o.Annotations[kwerftv1.AnnotationHold] == "" && olderUpgrade(&o, u) {
			r.wait(u, "Waiting behind "+o.Name+".")
			return ctrl.Result{}, nil
		}
	}
	if auto(u) {
		open, next, err := r.window(ctx)
		if err != nil {
			r.wait(u, "Waiting for a valid maintenance window: "+err.Error())
			return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
		}
		if !open {
			r.wait(u, "Waiting for the maintenance window.")
			if next.IsZero() {
				return ctrl.Result{RequeueAfter: time.Hour}, nil
			}
			return ctrl.Result{RequeueAfter: max(next.Sub(r.now()), time.Second)}, nil
		}
	}
	if u.Spec.Component == kwerftv1.UpgradeKubernetes && r.Kubernetes == nil {
		r.fail(u, "Preflight", "Kubernetes upgrades are not available in this release.")
		return ctrl.Result{}, nil
	}
	u.Status.Phase = kwerftv1.UpgradePreflight
	u.Status.StartedAt = &metav1.Time{Time: r.now()}
	u.Status.Reason, u.Status.Message = "", "Checking."
	u.Status.From = r.from(ctx)
	return ctrl.Result{Requeue: true}, nil
}

// holdsRunner: a Kwerft Upgrade from Backup (the Job is created then)
// until it finished. Kubernetes upgrades (U4) hold nothing here.
func holdsRunner(u *kwerftv1.Upgrade) bool {
	return u.Spec.Component == kwerftv1.UpgradeKwerft &&
		(u.Status.Phase == kwerftv1.UpgradeBackingUp || upgrades.RunnerStarted(u.Status.Phase))
}

func olderUpgrade(a, b *kwerftv1.Upgrade) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

func auto(u *kwerftv1.Upgrade) bool {
	return u.Annotations[kwerftv1.AnnotationRequestedBy] == kwerftv1.RequestedByAutoUpdate
}

// window: is the maintenance window open, and when does the next open.
func (r *UpgradeReconciler) window(ctx context.Context) (bool, time.Time, error) {
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, err
	}
	if s.Spec.Updates == nil {
		return false, time.Time{}, nil
	}
	w, err := upgrades.ParseWindow(s.Spec.Updates.Window)
	if err != nil {
		return false, time.Time{}, err
	}
	now := r.now()
	open, _ := w.Open(now)
	return open, w.Next(now), nil
}

func (r *UpgradeReconciler) wait(u *kwerftv1.Upgrade, msg string) {
	u.Status.Phase = kwerftv1.UpgradeQueued
	u.Status.Message = msg
}

func (r *UpgradeReconciler) cancel(u *kwerftv1.Upgrade, by string) {
	u.Status.Phase = kwerftv1.UpgradeCancelled
	u.Status.Reason = "Cancelled"
	u.Status.Message = "Cancelled by " + by + " before anything changed."
	u.Status.FinishedAt = &metav1.Time{Time: r.now()}
}

func (r *UpgradeReconciler) fail(u *kwerftv1.Upgrade, reason, msg string) {
	u.Status.Phase = kwerftv1.UpgradeFailed
	u.Status.Reason, u.Status.Message = reason, msg
	u.Status.FinishedAt = &metav1.Time{Time: r.now()}
}

// from: the versions running before the upgrade.
func (r *UpgradeReconciler) from(ctx context.Context) *kwerftv1.UpgradeVersions {
	v := &kwerftv1.UpgradeVersions{}
	if r.Checks != nil {
		v.Kwerft = r.Checks.Version
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err == nil {
		_, v.Kubernetes = kubeletVersions(nodes.Items)
	}
	return v
}

// beforePreflight handles what ends or delays an Upgrade in Preflight
// before its checks run, for either component: a cancel, and an
// auto-update whose window closed.
func (r *UpgradeReconciler) beforePreflight(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, bool) {
	if by := u.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
		r.cancel(u, by)
		return ctrl.Result{}, true
	}
	if auto(u) {
		if open, _, err := r.window(ctx); err != nil || !open {
			// Not started within the window: wait for the next one.
			r.wait(u, "Waiting for the next maintenance window.")
			return ctrl.Result{Requeue: true}, true
		}
	}
	return ctrl.Result{}, false
}

func (r *UpgradeReconciler) preflight(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	if res, stop := r.beforePreflight(ctx, u); stop {
		return res, nil
	}
	if r.Checks == nil {
		r.fail(u, "Preflight", "Kwerft upgrades are not available here.")
		return ctrl.Result{}, nil
	}
	checks := r.Checks.Kwerft(ctx, u.Spec, u.Name)
	u.Status.Preflight = checks
	if blocked := Blocked(checks); len(blocked) > 0 {
		var msgs []string
		for _, c := range blocked {
			msgs = append(msgs, c.Check+": "+c.Message)
		}
		r.fail(u, "Preflight", "Preflight failed. "+strings.Join(msgs, " "))
		return ctrl.Result{}, nil
	}
	u.Status.Phase = kwerftv1.UpgradeBackingUp
	u.Status.Message = "Backing up."
	return ctrl.Result{Requeue: true}, nil
}

// backup copies the database and starts the runner, which takes the etcd
// snapshot and the Helm revisions.
func (r *UpgradeReconciler) backup(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	if by := u.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
		// The runner checks the same annotation before it starts the
		// installer; the phase's optimistic lock decides who was first.
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: RunnerJobName(u.Name), Namespace: r.ns()}}
		if err := client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
			return ctrl.Result{}, err
		}
		r.cancel(u, by)
		return ctrl.Result{}, nil
	}
	if r.Database != nil && (u.Status.Backup == nil || u.Status.Backup.Database == "") {
		path, err := upgrades.BackupDatabase(ctx, r.Database, r.DataDir, u.Name)
		if err != nil {
			r.fail(u, "Backup", "The database copy failed: "+err.Error()+". Nothing was changed.")
			return ctrl.Result{}, nil
		}
		if u.Status.Backup == nil {
			u.Status.Backup = &kwerftv1.UpgradeBackup{}
		}
		u.Status.Backup.Database = path
	}
	return r.observeRunner(ctx, u)
}

// observeRunner makes sure the runner Job exists while the Upgrade is the
// runner's, and records a runner that gave up.
func (r *UpgradeReconciler) observeRunner(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: r.ns(), Name: RunnerJobName(u.Name)}, &job)
	switch {
	case apierrors.IsNotFound(err):
		if u.Spec.Component != kwerftv1.UpgradeKwerft || upgrades.Finished(u.Status.Phase) {
			return ctrl.Result{}, nil
		}
		// A runner Job missing later (deleted by hand) is created again:
		// the runner resumes from the status.
		if err := r.createRunner(ctx, u); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	case err != nil:
		return ctrl.Result{}, err
	}
	if reason, msg := jobFailure(&job); reason != "" && !upgrades.Finished(u.Status.Phase) {
		if upgrades.RunnerStarted(u.Status.Phase) {
			msg += " The installer may still run on the installer node: see `systemctl status " + upgrades.UnitName(u.Name) +
				"` and /var/log/kwerft/install.log there."
		} else {
			msg += " Nothing was changed."
		}
		r.fail(u, reason, msg)
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// createRunner creates u's runner Job. Without an image it fails u while
// nothing changed yet (Backup), and returns an error after that.
func (r *UpgradeReconciler) createRunner(ctx context.Context, u *kwerftv1.Upgrade) error {
	image := r.RunnerImage
	if u.Spec.Component == kwerftv1.UpgradeKubernetes && r.KubernetesRunnerImage != nil {
		image = r.KubernetesRunnerImage
	}
	if image == nil {
		r.fail(u, "Runner", "The upgrade runner's image is unknown.")
		return nil
	}
	ref, err := image(ctx)
	if err != nil {
		if u.Status.Phase == kwerftv1.UpgradeBackingUp {
			r.fail(u, "Runner", "Cannot pin the console image for the runner: "+err.Error()+". Nothing was changed.")
			return nil
		}
		return err
	}
	if err := r.Create(ctx, r.runnerJob(u, ref)); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	log.FromContext(ctx).Info("upgrade runner started", "upgrade", u.Name, "image", ref)
	return nil
}

// EnsureRunner returns u's runner Job, creating it when missing (a
// Kubernetes upgrade's etcd snapshot). It returns nil right after creating
// it, and when it could not start, in which case u is Failed.
func (r *UpgradeReconciler) EnsureRunner(ctx context.Context, u *kwerftv1.Upgrade) (*batchv1.Job, error) {
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: r.ns(), Name: RunnerJobName(u.Name)}, &job)
	switch {
	case apierrors.IsNotFound(err):
		return nil, r.createRunner(ctx, u)
	case err != nil:
		return nil, err
	}
	return &job, nil
}

// DeleteRunner removes u's runner Job (a cancel in Backup).
func (r *UpgradeReconciler) DeleteRunner(ctx context.Context, u *kwerftv1.Upgrade) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: RunnerJobName(u.Name), Namespace: r.ns()}}
	return client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

// finished: drop the finalizer, pause AutoPatch after an auto-update that
// changed something and then failed or rolled back (a failed preflight or
// backup changed nothing and leaves AutoPatch on), and keep only the newest
// Upgrades.
func (r *UpgradeReconciler) finished(ctx context.Context, u *kwerftv1.Upgrade) error {
	if controllerutil.ContainsFinalizer(u, upgradeFinalizer) {
		controllerutil.RemoveFinalizer(u, upgradeFinalizer)
		if err := r.Update(ctx, u); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	if auto(u) && (u.Status.Phase == kwerftv1.UpgradeFailed || u.Status.Phase == kwerftv1.UpgradeRolledBack) &&
		upgrades.ReachedRunning(u) && !meta.IsStatusConditionTrue(u.Status.Conditions, ConditionAutoPatchPaused) {
		if err := r.pauseAutoPatch(ctx, u.Name); err != nil {
			return err
		}
		orig := u.DeepCopy()
		meta.SetStatusCondition(&u.Status.Conditions, metav1.Condition{Type: ConditionAutoPatchPaused, Status: metav1.ConditionTrue,
			Reason: string(u.Status.Phase), Message: "AutoPatch waits until an owner resumes it.", ObservedGeneration: u.Generation})
		if err := patchStatus(ctx, r.Client, u, orig); err != nil {
			return err
		}
	}
	return r.retain(ctx)
}

func (r *UpgradeReconciler) pauseAutoPatch(ctx context.Context, name string) error {
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		return client.IgnoreNotFound(err)
	}
	orig := s.DeepCopy()
	if s.Status.Updates == nil {
		s.Status.Updates = &kwerftv1.UpdatesStatus{}
	}
	s.Status.Updates.AutoPatchPausedBy = name
	log.FromContext(ctx).Info("AutoPatch paused", "upgrade", name)
	return patchStatus(ctx, r.Client, &s, orig)
}

// retain deletes finished Upgrades beyond the newest Keep, with their log
// ConfigMaps and runner Jobs (garbage collection would too; envtest and a
// stalled collector do not).
func (r *UpgradeReconciler) retain(ctx context.Context) error {
	var list kwerftv1.UpgradeList
	if err := r.APIReader.List(ctx, &list); err != nil {
		return err
	}
	keep := r.Keep
	if keep <= 0 {
		keep = KeepUpgrades
	}
	items := list.Items
	slices.SortFunc(items, func(a, b kwerftv1.Upgrade) int {
		if olderUpgrade(&a, &b) {
			return 1
		}
		if olderUpgrade(&b, &a) {
			return -1
		}
		return 0
	})
	for i := keep; i < len(items); i++ {
		u := &items[i]
		if !upgrades.Finished(u.Status.Phase) || !u.DeletionTimestamp.IsZero() {
			continue
		}
		for _, obj := range []client.Object{
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: upgrades.LogConfigMapName(u.Name), Namespace: r.ns()}},
			&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: RunnerJobName(u.Name), Namespace: r.ns()}},
		} {
			if err := deleteIfOwnedByUpgrade(ctx, r.Client, obj, u); err != nil {
				return err
			}
		}
		if err := r.Delete(ctx, u); client.IgnoreNotFound(err) != nil {
			return err
		}
		log.FromContext(ctx).Info("old upgrade removed", "upgrade", u.Name)
	}
	return nil
}

func deleteIfOwnedByUpgrade(ctx context.Context, c client.Client, obj client.Object, u *kwerftv1.Upgrade) error {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == u.UID {
			return client.IgnoreNotFound(c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)))
		}
	}
	return nil
}

// GenerateName is the generateName of an Upgrade: <component>-<version>-.
func GenerateName(component kwerftv1.UpgradeComponent, version string) string {
	v := strings.ToLower(strings.NewReplacer("+", "-", "_", "-").Replace(version))
	name := fmt.Sprintf("%s-%s-", strings.ToLower(string(component)), v)
	if len(name) > 57 {
		name = name[:56] + "-"
	}
	return name
}
