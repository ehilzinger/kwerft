package controllers

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/backups"
)

// Restores: a project, or some of its Apps, back from a Velero backup.
//
//  1. The target Project (spec.targetProject, else the original) must
//     exist; when it does not, the reconciler creates it with the
//     original's spec as the backup holds it (or as the original has it
//     now, when the backup's contents cannot be read), and waits for its
//     namespace.
//  2. With spec.apps, the objects those Apps need that do not carry the
//     label kwerft.dev/app — shared Volumes, the Secrets and SecretSets they
//     reference, the App's own SecretSet <app>-env — are created from the
//     backup's contents first, where missing.
//  3. A Velero Restore "kwerft-<restore>" of the project's namespace
//     (namespaceMapping to the target; a selector on kwerft.dev/app for
//     Apps; existingResourcePolicy none, so nothing that exists is
//     overwritten; no cluster-scoped objects). Volume data comes back
//     through Velero's file system restore.
//  4. With spec.apps, the App objects themselves follow once Velero is done
//     (they carry no labels; their workloads, disks and Domains do): the
//     App reconciler then adopts what Velero restored.
//
// The backup's contents are read through a Velero DownloadRequest
// (BackupContents), only when step 1 or 2 needs them. The tarball is
// encrypted in the bucket (SSE-C): Velero signs the URL for the key, and
// the GET sends it from velero/kwerft-bsl-encryption.

const (
	restorePoll = 3 * time.Second
	// contentsWait is how long Velero may take to hand out the contents.
	contentsWait = 2 * time.Minute
	// DefaultContentsLimit bounds a backup's contents (uncompressed).
	DefaultContentsLimit = 512 << 20
)

// Restore phases.
const (
	RestorePending         = "Pending"
	RestoreInProgress      = "InProgress"
	RestoreCompleted       = "Completed"
	RestorePartiallyFailed = "PartiallyFailed"
	RestoreFailed          = "Failed"
)

// RestoreDone reports whether a Restore's phase is final.
func RestoreDone(phase string) bool {
	return phase == RestoreCompleted || phase == RestorePartiallyFailed || phase == RestoreFailed
}

// RestoreReconciler: see above.
type RestoreReconciler struct {
	client.Client
	// APIReader reads the backups' SSE-C key Secret uncached; nil falls
	// back to the client.
	APIReader client.Reader
	// HTTP downloads backup contents from Velero's signed URL; nil uses a
	// client with a 2-minute timeout.
	HTTP *http.Client
	// ContentsLimit bounds the contents read; 0 means DefaultContentsLimit.
	ContentsLimit int64
	Now           func() time.Time

	mu       sync.Mutex
	contents map[types.UID]*backups.Contents
}

func (r *RestoreReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *RestoreReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// restoreFailure ends a Restore with a message.
type restoreFailure struct{ msg string }

func (e *restoreFailure) Error() string { return e.msg }

func failRestore(format string, args ...any) error {
	return &restoreFailure{msg: fmt.Sprintf(format, args...)}
}

// errWait: a step waits for something (a namespace, Velero's download).
var errWait = errors.New("waiting")

// VeleroRestoreName is the Velero Restore of a Restore.
func VeleroRestoreName(restore string) string { return "kwerft-" + restore }

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rs kwerftv1.Restore
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if RestoreDone(rs.Status.Phase) || !rs.DeletionTimestamp.IsZero() {
		r.forget(rs.UID)
		return ctrl.Result{}, nil
	}
	orig := rs.DeepCopy()
	st := &rs.Status
	if st.Phase == "" {
		st.Phase = RestorePending
		st.StartedAt = &metav1.Time{Time: r.now()}
	}
	result, err := r.step(ctx, &rs)
	var failure *restoreFailure
	switch {
	case errors.As(err, &failure):
		st.Phase, st.Message = RestoreFailed, failure.msg
		err = nil
	case errors.Is(err, errWait):
		result, err = ctrl.Result{RequeueAfter: restorePoll}, nil
	case meta.IsNoMatchError(err):
		st.Phase, st.Message = RestoreFailed, "Velero is not installed: re-run the installer (stage Backups; --lite leaves it out)."
		err = nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if RestoreDone(st.Phase) {
		if st.CompletedAt == nil {
			st.CompletedAt = &metav1.Time{Time: r.now()}
		}
		r.forget(rs.UID)
		log.FromContext(ctx).Info("restore finished", "restore", rs.Name, "phase", st.Phase)
	}
	reason := st.Phase
	status := metav1.ConditionFalse
	if st.Phase == RestoreCompleted {
		status = metav1.ConditionTrue
	}
	setReady(&st.Conditions, rs.Generation, status, reason, cmp.Or(st.Message, restoreMessage(st.Phase)))
	if !equality.Semantic.DeepEqual(orig.Status, rs.Status) {
		if err := patchStatus(ctx, r.Client, &rs, orig); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return result, nil
}

func restoreMessage(phase string) string {
	switch phase {
	case RestorePending:
		return "Preparing the restore."
	case RestoreInProgress:
		return "Velero is restoring."
	case RestoreCompleted:
		return "Restored."
	case RestorePartiallyFailed:
		return "Restored with errors; Velero's restore log says which objects failed."
	}
	return "The restore failed."
}

// step moves the restore along; it writes the status into rs.
func (r *RestoreReconciler) step(ctx context.Context, rs *kwerftv1.Restore) (ctrl.Result, error) {
	vr := newVelero(VeleroRestoreGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: VeleroRestoreName(rs.Name)}, vr)
	if err == nil {
		return r.follow(ctx, rs, vr)
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	backup := newVelero(VeleroBackupGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: rs.Spec.Backup}, backup); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, failRestore("The backup %s does not exist (any more).", rs.Spec.Backup)
		}
		return ctrl.Result{}, err
	}
	b := ReadVeleroBackup(backup)
	if !BackupRestorable(b.Phase) {
		return ctrl.Result{}, failRestore("The backup %s is %s; only completed backups can be restored.", b.Name, b.Phase)
	}
	if !BackupHolds(b, rs.Spec.Project) {
		return ctrl.Result{}, failRestore("The backup %s does not hold the project %s.", b.Name, rs.Spec.Project)
	}
	target := cmp.Or(rs.Spec.TargetProject, rs.Spec.Project)
	if err := r.ensureProject(ctx, rs, target); err != nil {
		return ctrl.Result{}, err
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: target}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			rs.Status.Message = "Waiting for the project " + target + " to be set up."
			return ctrl.Result{}, errWait
		}
		return ctrl.Result{}, err
	}
	if len(rs.Spec.Apps) > 0 {
		if err := r.prepareApps(ctx, rs, target); err != nil {
			return ctrl.Result{}, err
		}
	}

	vr = newVelero(VeleroRestoreGVK)
	vr.SetName(VeleroRestoreName(rs.Name))
	vr.SetNamespace(VeleroNamespace)
	vr.SetLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelRestore: rs.Name})
	spec := map[string]any{
		"backupName":              rs.Spec.Backup,
		"includedNamespaces":      []any{rs.Spec.Project},
		"includeClusterResources": false,
		"existingResourcePolicy":  "none",
		"restorePVs":              true,
	}
	if target != rs.Spec.Project {
		spec["namespaceMapping"] = map[string]any{rs.Spec.Project: target}
	}
	if len(rs.Spec.Apps) > 0 {
		spec["labelSelector"] = map[string]any{"matchExpressions": []any{map[string]any{
			"key": LabelApp, "operator": "In", "values": toAny(sortedCopy(rs.Spec.Apps)),
		}}}
	}
	vr.Object["spec"] = spec
	if err := r.Create(ctx, vr); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	rs.Status.Phase = RestoreInProgress
	rs.Status.VeleroRestore = vr.GetName()
	rs.Status.Message = ""
	return ctrl.Result{RequeueAfter: restorePoll}, nil
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// follow mirrors the Velero Restore and finishes the restore.
func (r *RestoreReconciler) follow(ctx context.Context, rs *kwerftv1.Restore, vr *unstructured.Unstructured) (ctrl.Result, error) {
	st := &rs.Status
	st.VeleroRestore = vr.GetName()
	st.Warnings = int32(min(nestedInt(vr, "status", "warnings"), 1<<31-1))
	st.Errors = int32(min(nestedInt(vr, "status", "errors"), 1<<31-1))
	phase := nestedString(vr, "status", "phase")
	switch phase {
	case "Completed", "PartiallyFailed":
		if len(rs.Spec.Apps) > 0 {
			if err := r.createApps(ctx, rs, cmp.Or(rs.Spec.TargetProject, rs.Spec.Project)); err != nil {
				return ctrl.Result{}, err
			}
		}
		st.Phase, st.Message = phase, ""
		if t := nestedTime(vr, "status", "completionTimestamp"); t != nil {
			st.CompletedAt = &metav1.Time{Time: *t}
		}
		return ctrl.Result{}, nil
	case "Failed", "FailedValidation":
		msg := nestedString(vr, "status", "failureReason")
		if errs, _, _ := unstructured.NestedStringSlice(vr.Object, "status", "validationErrors"); msg == "" && len(errs) > 0 {
			msg = strings.Join(errs, "; ")
		}
		return ctrl.Result{}, failRestore("Velero could not restore: %s", cmp.Or(msg, "no reason given"))
	}
	st.Phase = RestoreInProgress
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// ensureProject creates the target Project when it is missing.
func (r *RestoreReconciler) ensureProject(ctx context.Context, rs *kwerftv1.Restore, target string) error {
	var p kwerftv1.Project
	err := r.Get(ctx, client.ObjectKey{Name: target}, &p)
	if err == nil {
		if !p.DeletionTimestamp.IsZero() {
			return failRestore("The project %s is being deleted; restore under another name.", target)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	spec, err := r.projectSpec(ctx, rs)
	if err != nil {
		return err
	}
	p = kwerftv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: target, Annotations: map[string]string{AnnotationRestoredBy: rs.Name}},
		Spec:       *spec,
	}
	if err := r.Create(ctx, &p); err != nil && !apierrors.IsAlreadyExists(err) {
		if apierrors.IsInvalid(err) {
			return failRestore("The project %s cannot be created: %s", target, err.Error())
		}
		return err
	}
	log.FromContext(ctx).Info("created the project of a restore", "restore", rs.Name, "project", target)
	rs.Status.Message = "Waiting for the project " + target + " to be set up."
	return errWait
}

// projectSpec is the original project's spec: the backup's, or the live
// one when the backup's contents cannot be read.
func (r *RestoreReconciler) projectSpec(ctx context.Context, rs *kwerftv1.Restore) (*kwerftv1.ProjectSpec, error) {
	c, err := r.backupContents(ctx, rs)
	if errors.Is(err, errWait) {
		return nil, err
	}
	if err == nil {
		if raw, ok := c.Get("projects.kwerft.dev", "", rs.Spec.Project); ok {
			var p kwerftv1.Project
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, failRestore("The backup's copy of the project %s cannot be read: %v", rs.Spec.Project, err)
			}
			return &p.Spec, nil
		}
	}
	var live kwerftv1.Project
	if gerr := r.Get(ctx, client.ObjectKey{Name: rs.Spec.Project}, &live); gerr == nil {
		return &live.Spec, nil
	}
	if err != nil {
		return nil, failRestore("The backup's contents could not be read (%v), and the project %s no longer exists to copy its settings from.", err, rs.Spec.Project)
	}
	return nil, failRestore("The backup does not hold the project %s's settings.", rs.Spec.Project)
}

// appRefs are what an App refers to by name in its project.
type appRefs struct{ volumes, secrets, configMaps []string }

func refsOf(app *kwerftv1.App) appRefs {
	var r appRefs
	for _, v := range app.Spec.Volumes {
		if v.Volume != "" {
			r.volumes = append(r.volumes, v.Volume)
		}
		if v.Secret != "" {
			r.secrets = append(r.secrets, v.Secret)
		}
	}
	for _, e := range app.Spec.Env {
		if e.ValueFrom == nil {
			continue
		}
		if ref := e.ValueFrom.SecretKeyRef; ref != nil && ref.Name != "" {
			r.secrets = append(r.secrets, ref.Name)
		}
		if ref := e.ValueFrom.ConfigMapKeyRef; ref != nil && ref.Name != "" {
			r.configMaps = append(r.configMaps, ref.Name)
		}
	}
	if img := app.Spec.Source.Image; img != nil && img.PullSecret != "" {
		r.secrets = append(r.secrets, img.PullSecret)
	}
	// The App's own SecretSet and its Secret (docs/plan.md › Secrets).
	r.secrets = append(r.secrets, app.Name+"-env")
	for _, l := range []*[]string{&r.volumes, &r.secrets, &r.configMaps} {
		slices.Sort(*l)
		*l = slices.Compact(*l)
	}
	return r
}

func (r *RestoreReconciler) backupApps(ctx context.Context, rs *kwerftv1.Restore) (*backups.Contents, []*kwerftv1.App, error) {
	c, err := r.backupContents(ctx, rs)
	if errors.Is(err, errWait) {
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, failRestore("The backup's contents could not be read: %v", err)
	}
	var apps []*kwerftv1.App
	var missing []string
	for _, name := range sortedCopy(rs.Spec.Apps) {
		raw, ok := c.Get("apps.kwerft.dev", rs.Spec.Project, name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		var app kwerftv1.App
		if err := json.Unmarshal(raw, &app); err != nil {
			return nil, nil, failRestore("The backup's copy of the app %s cannot be read: %v", name, err)
		}
		apps = append(apps, &app)
	}
	if len(missing) > 0 {
		return nil, nil, failRestore("The backup does not hold the %s %s in the project %s.",
			plural(len(missing), "app", "apps"), strings.Join(missing, ", "), rs.Spec.Project)
	}
	return c, apps, nil
}

// prepareApps creates what the Apps refer to and Velero's label selector
// does not catch.
func (r *RestoreReconciler) prepareApps(ctx context.Context, rs *kwerftv1.Restore, target string) error {
	c, apps, err := r.backupApps(ctx, rs)
	if err != nil {
		return err
	}
	for _, app := range apps {
		refs := refsOf(app)
		// Secrets before their SecretSets: the SecretSet reconciler then
		// finds the values.
		for _, name := range refs.secrets {
			if err := r.copyObject(ctx, c, "secrets", corev1.SchemeGroupVersion.WithKind("Secret"), rs.Spec.Project, name, target); err != nil {
				return err
			}
			if err := r.copyObject(ctx, c, "secretsets.kwerft.dev", kwerftv1.GroupVersion.WithKind("SecretSet"), rs.Spec.Project, name, target); err != nil {
				return err
			}
		}
		for _, name := range refs.configMaps {
			if err := r.copyObject(ctx, c, "configmaps", corev1.SchemeGroupVersion.WithKind("ConfigMap"), rs.Spec.Project, name, target); err != nil {
				return err
			}
		}
		for _, name := range refs.volumes {
			if err := r.copyObject(ctx, c, "volumes.kwerft.dev", kwerftv1.GroupVersion.WithKind("Volume"), rs.Spec.Project, name, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// createApps creates the App objects once Velero restored their workloads.
func (r *RestoreReconciler) createApps(ctx context.Context, rs *kwerftv1.Restore, target string) error {
	c, apps, err := r.backupApps(ctx, rs)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if err := r.copyObject(ctx, c, "apps.kwerft.dev", kwerftv1.GroupVersion.WithKind("App"), rs.Spec.Project, app.Name, target); err != nil {
			return err
		}
	}
	return nil
}

// copyObject creates an object from the backup in the target namespace,
// unless it is not in the backup or already exists (never overwritten).
func (r *RestoreReconciler) copyObject(ctx context.Context, c *backups.Contents, resource string, gvk schema.GroupVersionKind, namespace, name, target string) error {
	raw, ok := c.Get(resource, namespace, name)
	if !ok {
		return nil
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(raw); err != nil {
		return failRestore("The backup's copy of %s %s cannot be read: %v", strings.ToLower(gvk.Kind), name, err)
	}
	clean := newVelero(gvk)
	clean.SetName(name)
	clean.SetNamespace(target)
	clean.SetLabels(u.GetLabels())
	ann := u.GetAnnotations()
	delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	if len(ann) > 0 {
		clean.SetAnnotations(ann)
	}
	for k, v := range u.Object {
		switch k {
		case "apiVersion", "kind", "metadata", "status":
		default:
			clean.Object[k] = v
		}
	}
	err := r.Create(ctx, clean)
	switch {
	case err == nil:
		log.FromContext(ctx).Info("restored from a backup", "kind", gvk.Kind, "namespace", target, "name", name)
		return nil
	case apierrors.IsAlreadyExists(err):
		return nil
	case apierrors.IsInvalid(err):
		return failRestore("%s %s from the backup is not valid any more: %v", gvk.Kind, name, err)
	}
	return err
}

// ---- the backup's contents ------------------------------------------------------------

func (r *RestoreReconciler) forget(uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.contents, uid)
}

// backupContents reads the backup's contents through a DownloadRequest;
// errWait while Velero prepares the download.
func (r *RestoreReconciler) backupContents(ctx context.Context, rs *kwerftv1.Restore) (*backups.Contents, error) {
	r.mu.Lock()
	c := r.contents[rs.UID]
	r.mu.Unlock()
	if c != nil {
		return c, nil
	}
	name := VeleroRestoreName(rs.Name) + "-contents"
	dr := newVelero(VeleroDownloadRequestGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: name}, dr)
	expired := err == nil && nestedTime(dr, "status", "expiration") != nil && nestedTime(dr, "status", "expiration").Before(r.now())
	if apierrors.IsNotFound(err) || expired {
		if expired {
			if err := client.IgnoreNotFound(r.Delete(ctx, dr)); err != nil {
				return nil, err
			}
		}
		dr = newVelero(VeleroDownloadRequestGVK)
		dr.SetName(name)
		dr.SetNamespace(VeleroNamespace)
		dr.SetLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelRestore: rs.Name})
		dr.Object["spec"] = map[string]any{"target": map[string]any{"kind": "BackupContents", "name": rs.Spec.Backup}}
		if err := r.Create(ctx, dr); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		rs.Status.Message = "Reading the backup's contents."
		return nil, errWait
	}
	if err != nil {
		return nil, err
	}
	downloadURL := nestedString(dr, "status", "downloadURL")
	if nestedString(dr, "status", "phase") != "Processed" || downloadURL == "" {
		if r.now().Sub(dr.GetCreationTimestamp().Time) > contentsWait {
			_ = r.Delete(ctx, dr)
			return nil, errors.New("Velero did not hand out the backup's contents within 2 minutes")
		}
		rs.Status.Message = "Reading the backup's contents."
		return nil, errWait
	}
	c, err = r.download(ctx, downloadURL)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.contents == nil {
		r.contents = map[types.UID]*backups.Contents{}
	}
	r.contents[rs.UID] = c
	r.mu.Unlock()
	_ = client.IgnoreNotFound(r.Delete(ctx, dr))
	return c, nil
}

func (r *RestoreReconciler) download(ctx context.Context, u string) (*backups.Contents, error) {
	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if backups.PresignedWithSSEC(u) {
		// Velero signed the URL for the location's SSE-C key; the key
		// itself goes in headers, as the plugin sent it.
		var sec corev1.Secret
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: BackupEncryptionSecret}, &sec); err != nil {
			return nil, fmt.Errorf("reading the backups' encryption key: %w", err)
		}
		key := sec.Data[BackupEncryptionSecretKey]
		if len(key) != backups.SSEKeyBytes {
			return nil, errors.New("the backups' encryption key (velero/" + BackupEncryptionSecret + ") is missing")
		}
		backups.SetSSEC(req.Header, key)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("the storage answered %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	limit := r.ContentsLimit
	if limit == 0 {
		limit = DefaultContentsLimit
	}
	return backups.ReadContents(resp.Body, limit)
}

func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	byLabel := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if name := o.GetLabels()[LabelRestore]; name != "" && o.GetNamespace() == VeleroNamespace {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
		}
		return nil
	})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("restore").
		For(&kwerftv1.Restore{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	if veleroInstalled(mgr, VeleroRestoreGVK) {
		b = b.Watches(newVelero(VeleroRestoreGVK), byLabel, builder.WithPredicates(veleroPhaseChanged()))
	}
	if veleroInstalled(mgr, VeleroDownloadRequestGVK) {
		b = b.Watches(newVelero(VeleroDownloadRequestGVK), byLabel, builder.WithPredicates(veleroPhaseChanged()))
	}
	return b.Complete(r)
}
