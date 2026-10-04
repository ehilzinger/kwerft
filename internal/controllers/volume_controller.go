package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1ac "github.com/ehilzinger/kwerft/api/applyconfiguration/api/v1alpha1"
	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// LabelVolume marks the PersistentVolumeClaim of a Volume.
	LabelVolume = "kwerft.dev/volume"

	// VolumeFinalizer keeps a Volume (and so its claim and data) while Apps,
	// Schedules or unfinished Tasks still mount it.
	VolumeFinalizer = "kwerft.dev/volume-protection"

	// finalizerFieldOwner applies only the finalizer, so it never competes
	// with whoever owns the rest of the Volume.
	finalizerFieldOwner = "kwerft-finalizer"
)

// VolumeReconciler turns a Volume into a PersistentVolumeClaim of the same
// name and reports its phase, capacity and users.
//
// Deleting a Volume that is still mounted waits: the finalizer stays until no
// App, Schedule or unfinished Task refers to it, and the Ready condition says
// who does (reason InUse). Only then is the claim garbage-collected, and the
// pvc-protection finalizer of Kubernetes itself still waits for pods that
// have it mounted. So storage never disappears under a declared user.
type VolumeReconciler struct {
	client.Client
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vol kwerftv1.Volume
	if err := r.Get(ctx, req.NamespacedName, &vol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := vol.DeepCopy()

	users, err := volumeUsers(ctx, r.Client, vol.Namespace, vol.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	vol.Status.UsedBy = users

	if !vol.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&vol, VolumeFinalizer) {
			return ctrl.Result{}, nil
		}
		if len(users) > 0 {
			setReady(&vol.Status.Conditions, vol.Generation, metav1.ConditionFalse, "InUse",
				"Deletion waits until it is no longer mounted by "+strings.Join(users, ", "))
			return ctrl.Result{}, r.patchStatus(ctx, orig, &vol)
		}
		// Release: the claim is garbage-collected through its owner reference.
		return ctrl.Result{}, r.setFinalizer(ctx, &vol, false)
	}

	if !controllerutil.ContainsFinalizer(&vol, VolumeFinalizer) {
		if err := r.setFinalizer(ctx, &vol, true); err != nil {
			return ctrl.Result{}, err
		}
	}

	ready, err := r.reconcile(ctx, &vol)
	switch {
	case err != nil:
		setReady(&vol.Status.Conditions, vol.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
	default:
		setReady(&vol.Status.Conditions, vol.Generation, ready.status, ready.reason, ready.message)
	}
	vol.Status.ObservedGeneration = vol.Generation
	if perr := r.patchStatus(ctx, orig, &vol); perr != nil {
		return ctrl.Result{}, perr
	}
	if isTerminal(err) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, err
}

func (r *VolumeReconciler) reconcile(ctx context.Context, vol *kwerftv1.Volume) (*readiness, error) {
	project, err := projectOf(ctx, r.Client, vol.Namespace)
	if err != nil {
		return nil, err
	}

	// Never take over a claim someone else made.
	claim := &corev1.PersistentVolumeClaim{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: vol.Namespace, Name: volumeClaimName(vol.Name)}, claim); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, err
	case !metav1.IsControlledBy(claim, vol):
		return nil, terminalf("ClaimConflict", "a PersistentVolumeClaim named %q already exists and does not belong to this Volume", claim.Name)
	}

	pvc := corev1ac.PersistentVolumeClaim(volumeClaimName(vol.Name), vol.Namespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelProject: project, LabelVolume: vol.Name}).
		WithOwnerReferences(controllerRef(vol, kwerftv1.GroupVersion.WithKind("Volume"))).
		WithSpec(corev1ac.PersistentVolumeClaimSpec().
			WithAccessModes(corev1.ReadWriteOnce).
			WithStorageClassName(storageClass(vol.Spec.Class)).
			WithResources(corev1ac.VolumeResourceRequirements().
				WithRequests(corev1.ResourceList{corev1.ResourceStorage: vol.Spec.Size})))
	if err := apply(ctx, r.Client, pvc); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			// e.g. growing a local-nvme volume: its storage class cannot resize.
			return nil, terminalf("ClaimRejected", "the claim was rejected: %v", err)
		}
		return nil, fmt.Errorf("apply claim: %w", err)
	}

	vol.Status.ClaimName = volumeClaimName(vol.Name)
	if err := r.Get(ctx, client.ObjectKey{Namespace: vol.Namespace, Name: vol.Status.ClaimName}, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return &readiness{metav1.ConditionFalse, "Pending", "Creating the claim"}, nil // cache not caught up yet
		}
		return nil, err
	}
	vol.Status.Phase = claim.Status.Phase
	vol.Status.Capacity = nil
	if c, ok := claim.Status.Capacity[corev1.ResourceStorage]; ok {
		vol.Status.Capacity = &c
	}
	switch claim.Status.Phase {
	case corev1.ClaimBound:
		return &readiness{metav1.ConditionTrue, "Bound", "The disk is provisioned"}, nil
	case corev1.ClaimLost:
		return &readiness{metav1.ConditionFalse, "Lost", "The disk behind the claim is gone"}, nil
	default:
		msg := "Waiting for the disk to be provisioned"
		if storageClass(vol.Spec.Class) == storageClasses["local-nvme"] {
			msg = "Provisioned on the node of the first pod that mounts it"
		}
		return &readiness{metav1.ConditionFalse, "Pending", msg}, nil
	}
}

func (r *VolumeReconciler) patchStatus(ctx context.Context, orig, vol *kwerftv1.Volume) error {
	if equality.Semantic.DeepEqual(orig.Status, vol.Status) {
		return nil
	}
	return r.Status().Patch(ctx, vol, client.MergeFrom(orig))
}

// setFinalizer adds or removes the volume-protection finalizer with its own
// field manager, so nothing else on the Volume changes hands.
func (r *VolumeReconciler) setFinalizer(ctx context.Context, vol *kwerftv1.Volume, present bool) error {
	cfg := kwerftv1ac.Volume(vol.Name, vol.Namespace).WithUID(vol.UID)
	if present {
		cfg.WithFinalizers(VolumeFinalizer)
	}
	return client.IgnoreNotFound(r.Apply(ctx, cfg, client.FieldOwner(finalizerFieldOwner), client.ForceOwnership))
}

// VolumeUsers is volumeUsers for the API, which asks with the user's client
// so a delete can say right away who keeps the Volume.
func VolumeUsers(ctx context.Context, c client.Client, namespace, name string) ([]string, error) {
	return volumeUsers(ctx, c, namespace, name)
}

// volumeUsers lists who mounts Volume name in namespace: Apps, Schedules and
// Tasks that have not finished, as "Kind/name", sorted. Objects already being
// deleted do not count (their pods are going; pvc-protection covers those).
// A Task with fromApp counts when its App mounts the Volume now.
func volumeUsers(ctx context.Context, c client.Client, namespace, name string) ([]string, error) {
	mounts := func(vols []kwerftv1.AppVolume) bool {
		return slices.ContainsFunc(vols, func(v kwerftv1.AppVolume) bool { return v.Volume == name })
	}
	var users []string

	var apps kwerftv1.AppList
	if err := c.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	appMounts := map[string]bool{}
	for _, a := range apps.Items {
		if mounts(a.Spec.Volumes) {
			appMounts[a.Name] = true
			if a.DeletionTimestamp.IsZero() {
				users = append(users, "App/"+a.Name)
			}
		}
	}

	var schedules kwerftv1.ScheduleList
	if err := c.List(ctx, &schedules, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for _, s := range schedules.Items {
		if s.DeletionTimestamp.IsZero() && (mounts(s.Spec.Task.Volumes) || appMounts[s.Spec.Task.FromApp]) {
			users = append(users, "Schedule/"+s.Name)
		}
	}

	var tasks kwerftv1.TaskList
	if err := c.List(ctx, &tasks, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for _, t := range tasks.Items {
		if t.DeletionTimestamp.IsZero() && !t.Status.Phase.Finished() && (mounts(t.Spec.Volumes) || appMounts[t.Spec.FromApp]) {
			users = append(users, "Task/"+t.Name)
		}
	}
	slices.Sort(users)
	return users, nil
}

// missingVolumes returns the shared Volumes in vols that do not exist.
func missingVolumes(ctx context.Context, c client.Client, namespace string, vols []kwerftv1.AppVolume) ([]string, error) {
	var missing []string
	for _, v := range vols {
		if v.Volume == "" {
			continue
		}
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: v.Volume}, &kwerftv1.Volume{})
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, v.Volume)
		case err != nil:
			return nil, err
		}
	}
	return missing, nil
}

func volumeClaimName(volume string) string { return volume }

// projectOf returns the project a namespace belongs to, or a terminal error.
func projectOf(ctx context.Context, c client.Client, namespace string) (string, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return "", err
	}
	if p := ns.Labels[LabelProject]; p != "" {
		return p, nil
	}
	return "", terminalf("NotInProject", "namespace %q is not a Kwerft project; create a Project first", namespace)
}

// volumesInNamespace enqueues every Volume in the object's namespace: a
// change to an App, Task or Schedule can change who uses which Volume.
func (r *VolumeReconciler) volumesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list kwerftv1.VolumeList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, v := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&v)})
	}
	return reqs
}

func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	users := handler.EnqueueRequestsFromMapFunc(r.volumesInNamespace)
	return ctrl.NewControllerManagedBy(mgr).
		For(&kwerftv1.Volume{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&kwerftv1.App{}, users).
		Watches(&kwerftv1.Task{}, users).
		Watches(&kwerftv1.Schedule{}, users).
		Named("volume").
		Complete(r)
}
