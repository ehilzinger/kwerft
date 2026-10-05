package controllers

import (
	"context"
	"fmt"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func createVolume(t *testing.T, ns, name, size, class string) *kwerftv1.Volume {
	t.Helper()
	vol := &kwerftv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       kwerftv1.VolumeSpec{Size: resource.MustParse(size), Class: class},
	}
	if err := k8s.Create(context.Background(), vol); err != nil {
		t.Fatal(err)
	}
	return vol
}

// waitForVolume waits until the Volume's Ready reason is want and returns it.
func waitForVolume(t *testing.T, vol *kwerftv1.Volume, want string) *kwerftv1.Volume {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(vol), vol); err != nil {
			return err
		}
		reason, err := readyReason(vol.Status.Conditions, vol.Generation)
		if err != nil {
			return err
		}
		if reason != want {
			return fmt.Errorf("reason %q, want %q", reason, want)
		}
		return nil
	})
	return vol
}

func TestVolumeBecomesClaim(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "vols")
	vol := createVolume(t, "vols", "media", "10Gi", "hcloud-volume")
	vol = waitForVolume(t, vol, "Pending")

	var pvc corev1.PersistentVolumeClaim
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "vols", Name: "media"}, &pvc); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&pvc, vol) {
		t.Error("claim is not controlled by the Volume")
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "hcloud-volumes" {
		t.Errorf("storageClassName = %v, want hcloud-volumes", pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("storage request = %s", got.String())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v", pvc.Spec.AccessModes)
	}
	if !controllerutil.ContainsFinalizer(vol, VolumeFinalizer) {
		t.Errorf("finalizers = %v, want %s", vol.Finalizers, VolumeFinalizer)
	}
	if vol.Status.ClaimName != "media" {
		t.Errorf("claimName = %q", vol.Status.ClaimName)
	}

	// Play the provisioner.
	pvc.Status.Phase = corev1.ClaimBound
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
	if err := k8s.Status().Update(ctx, &pvc); err != nil {
		t.Fatal(err)
	}
	vol = waitForVolume(t, vol, "Bound")
	if vol.Status.Phase != corev1.ClaimBound || vol.Status.Capacity == nil || vol.Status.Capacity.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("status = phase %q capacity %v", vol.Status.Phase, vol.Status.Capacity)
	}
}

func TestAppMountsSharedVolumeAsDeployment(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "shared")
	createVolume(t, "shared", "uploads", "5Gi", "")
	app := createApp(t, "shared", "web", kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.29"}},
		Volumes: []kwerftv1.AppVolume{{Path: "/srv/uploads", Volume: "uploads", ReadOnly: true}},
	})
	waitForApp(t, app, "Progressing")

	var d appsv1.Deployment
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "web"}, &d); err != nil {
		t.Fatalf("an App with only shared Volumes runs as a Deployment: %v", err)
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "shared", Name: "web"}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Errorf("no StatefulSet expected (err=%v)", err)
	}
	pod := d.Spec.Template
	// Read-only in the container only: a read-only claim would have the CSI
	// driver mount the device read-only on the node, and a Hetzner Cloud
	// Volume could then not be mounted read-write there by another pod.
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "uploads" || pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Errorf("pod volumes = %+v, want the claim mounted read-write", pod.Spec.Volumes)
	}
	m := pod.Spec.Containers[0].VolumeMounts
	if len(m) != 1 || m[0].MountPath != "/srv/uploads" || m[0].Name != pod.Spec.Volumes[0].Name || !m[0].ReadOnly {
		t.Errorf("volumeMounts = %+v", m)
	}
	if pod.Labels[LabelVolumePrefix+"uploads"] != "true" {
		t.Errorf("pod labels = %v, want the volume label", pod.Labels)
	}
	if a := pod.Spec.Affinity; a == nil || a.PodAffinity == nil || len(a.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 ||
		a.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm.TopologyKey != corev1.LabelHostname {
		t.Errorf("affinity = %+v, want co-location with other users of the Volume", a)
	}
	if d.Spec.Selector.MatchLabels[LabelVolumePrefix+"uploads"] != "" {
		t.Error("the volume label must not be part of the selector")
	}

	// The Volume knows its user.
	vol := &kwerftv1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: "shared", Name: "uploads"}}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(vol), vol); err != nil {
			return err
		}
		if !slices.Equal(vol.Status.UsedBy, []string{"App/web"}) {
			return fmt.Errorf("usedBy = %v", vol.Status.UsedBy)
		}
		return nil
	})
}

func TestAppWaitsForMissingVolume(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "later")
	app := createApp(t, "later", "worker", kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "busybox:1.37"}},
		Volumes: []kwerftv1.AppVolume{{Path: "/data", Volume: "inbox"}},
	})
	waitForApp(t, app, "VolumeNotFound")
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "later", Name: "worker"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("no workload expected before the Volume exists (err=%v)", err)
	}
	createVolume(t, "later", "inbox", "1Gi", "")
	waitForApp(t, app, "Progressing")
}

func TestVolumeDeletionWaitsForUsers(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "keep")
	vol := createVolume(t, "keep", "data", "1Gi", "")
	app := createApp(t, "keep", "db", kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "postgres:17.6"}},
		Volumes: []kwerftv1.AppVolume{{Path: "/var/lib/postgresql/data", Volume: "data"}},
	})
	app = waitForApp(t, app, "Progressing")
	waitForVolume(t, vol, "Pending")

	if err := k8s.Delete(ctx, vol); err != nil {
		t.Fatal(err)
	}
	vol = waitForVolume(t, vol, "InUse")
	if vol.DeletionTimestamp.IsZero() {
		t.Fatal("expected the Volume to be terminating")
	}

	// Unmount it: now the Volume goes.
	app.Spec.Volumes = nil
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		err := k8s.Get(ctx, client.ObjectKeyFromObject(vol), &kwerftv1.Volume{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("volume still there (err=%v)", err)
	})
}

func TestVolumeClaimConflict(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "clash")
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "clash", Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	vol := createVolume(t, "clash", "data", "2Gi", "")
	waitForVolume(t, vol, "ClaimConflict")
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
		t.Fatal(err)
	}
	if got := foreign.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("foreign claim was changed: %s", got.String())
	}
}
