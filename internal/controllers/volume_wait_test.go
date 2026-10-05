package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// The kubelet's message when a Hetzner Cloud Volume is already mounted
// read-only on the node (observed 2026-10-05).
const busyMount = `MountVolume.SetUp failed for volume "pvc-1f2e" : rpc error: code = Internal desc = failed to publish volume: mount failed: exit status 32
Mounting command: mount
Mounting arguments: -t ext4 -o defaults /dev/disk/by-id/scsi-0HC_Volume_101 /var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-1f2e/mount
Output: mount: mounting /dev/disk/by-id/scsi-0HC_Volume_101 on /var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-1f2e/mount failed: Resource busy`

// volumePod plays the ReplicaSet or Job controller and the scheduler: a pod
// on node with the given labels that mounts the claim.
func volumePod(t *testing.T, ns, name, node, claim string, readOnly bool, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "main", Image: "busybox:1.37"}},
			Volumes: []corev1.Volume{{Name: "v0", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: readOnly}}}},
		},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = phase
	if phase == corev1.PodPending {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", Image: "busybox:1.37",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
	}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

// podEvent plays the kubelet: an event about the pod.
func podEvent(t *testing.T, pod *corev1.Pod, name, reason, message string, at time.Time) {
	t.Helper()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: name},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", APIVersion: "v1",
			Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
		Reason: reason, Message: message, Type: corev1.EventTypeWarning,
		Source:         corev1.EventSource{Component: "kubelet", Host: pod.Spec.NodeName},
		FirstTimestamp: metav1.NewTime(at), LastTimestamp: metav1.NewTime(at), Count: 1,
	}
	if err := k8s.Create(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func TestAppSaysWhichPodsHoldItsVolumeReadOnly(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "tiles")
	createVolume(t, "tiles", "tiles", "10Gi", "hcloud-volume")
	app := createApp(t, "tiles", "writer", kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.29"}},
		Volumes: []kwerftv1.AppVolume{{Path: "/data", Volume: "tiles"}},
	})
	waitForApp(t, app, "Progressing")

	// Readers started by an earlier Kwerft hold the device read-only on
	// node-1; one on another node and a finished one do not count.
	volumePod(t, "tiles", "origin-7d9f", "node-1", "tiles", true, map[string]string{LabelApp: "origin"}, corev1.PodRunning)
	volumePod(t, "tiles", "tiles-5c4b", "node-1", "tiles", true, map[string]string{LabelApp: "tiles"}, corev1.PodRunning)
	volumePod(t, "tiles", "far-1a2b", "node-2", "tiles", true, map[string]string{LabelApp: "far"}, corev1.PodRunning)
	volumePod(t, "tiles", "done-3c4d", "node-1", "tiles", true, map[string]string{LabelTask: "done"}, corev1.PodSucceeded)
	stuck := volumePod(t, "tiles", "writer-6b8c", "node-1", "tiles", false, map[string]string{LabelApp: "writer"}, corev1.PodPending)
	podEvent(t, stuck, "writer-6b8c.1", "FailedMount", busyMount, time.Now())
	poke(t, app)

	want := "Volume tiles is mounted read-only on node node-1 by App origin, App tiles, started by an earlier Kwerft"
	eventually(t, func() error {
		c := appReady(app)
		if c == nil || c.Reason != "Progressing" || !strings.Contains(c.Message, want) {
			return fmt.Errorf("ready = %+v, want %q", c, want)
		}
		return nil
	})
}

func TestAppSaysWhyItsDiskDoesNotMount(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "mounts")
	createVolume(t, "mounts", "media", "10Gi", "hcloud-volume")
	app := createApp(t, "mounts", "web", kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.29"}},
		Volumes: []kwerftv1.AppVolume{{Path: "/media", Volume: "media", ReadOnly: true}},
	})
	waitForApp(t, app, "Progressing")

	stuck := volumePod(t, "mounts", "web-6b8c", "node-1", "media", false, map[string]string{LabelApp: "web"}, corev1.PodPending)
	now := time.Now()
	podEvent(t, stuck, "web-6b8c.1", "FailedAttachVolume",
		`AttachVolume.Attach failed for volume "pvc-9a" : rpc error: code = Unavailable desc = volume is attached to another server`, now.Add(-time.Minute))
	podEvent(t, stuck, "web-6b8c.2", "FailedMount",
		"Unable to attach or mount volumes: unmounted volumes=[v0], unattached volumes=[v0]: timed out waiting for the condition", now)
	poke(t, app)

	want := "0/1 replicas ready: waiting for a volume: AttachVolume.Attach failed for volume \"pvc-9a\""
	eventually(t, func() error {
		c := appReady(app)
		if c == nil || !strings.HasPrefix(c.Message, want) {
			return fmt.Errorf("ready = %+v, want %q", c, want)
		}
		return nil
	})
}

func appReady(app *kwerftv1.App) *metav1.Condition {
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(app), app); err != nil {
		return nil
	}
	return meta.FindStatusCondition(app.Status.Conditions, ConditionReady)
}

func TestTaskSaysWhyItsVolumeDoesNotMount(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "fill")
	createVolume(t, "fill", "tiles", "10Gi", "hcloud-volume")
	spec := imageTask("busybox:1.37")
	spec.Volumes = []kwerftv1.AppVolume{{Path: "/tiles", Volume: "tiles"}}
	task := createTask(t, "fill", "render", spec)
	job := waitForJob(t, "fill", "render")

	volumePod(t, "fill", "tiles-5c4b", "node-1", "tiles", true, map[string]string{LabelApp: "tiles"}, corev1.PodRunning)
	stuck := volumePod(t, "fill", "render-x7k2p", "node-1", "tiles", false, map[string]string{LabelTask: "render"}, corev1.PodPending)
	podEvent(t, stuck, "render-x7k2p.1", "FailedMount", busyMount, time.Now())
	poke(t, job)

	want := "Waiting for the pod to start: Volume tiles is mounted read-only on node node-1 by App tiles"
	waitForTask(t, task, func(task *kwerftv1.Task) error {
		c := meta.FindStatusCondition(task.Status.Conditions, ConditionReady)
		if c == nil || c.Reason != "Pending" || !strings.HasPrefix(c.Message, want) {
			return fmt.Errorf("ready = %+v, want %q", c, want)
		}
		return nil
	})
}

func TestTaskMountsReadOnlyVolumeReadWriteUnderneath(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "readers")
	createVolume(t, "readers", "archive", "1Gi", "hcloud-volume")
	spec := imageTask("busybox:1.37")
	spec.Volumes = []kwerftv1.AppVolume{{Path: "/archive", Volume: "archive", ReadOnly: true}}
	createTask(t, "readers", "scan", spec)
	pod := waitForJob(t, "readers", "scan").Spec.Template.Spec

	if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Errorf("volumes = %+v, want the claim read-write", pod.Volumes)
	}
	if m := pod.Containers[0].VolumeMounts; len(m) != 1 || !m[0].ReadOnly {
		t.Errorf("volumeMounts = %+v, want the container's mount read-only", m)
	}
}

func TestVolumeEventPrefersTheDriversError(t *testing.T) {
	at := func(s int) metav1.Time { return metav1.NewTime(time.Unix(1_800_000_000+int64(s), 0)) }
	events := []corev1.Event{
		{Reason: "Scheduled", Message: "Successfully assigned", LastTimestamp: at(0)},
		{Reason: "FailedMount", Message: "MountVolume.SetUp failed: older", LastTimestamp: at(1)},
		{Reason: "FailedMount", Message: "MountVolume.SetUp failed: newer", LastTimestamp: at(2)},
		{Reason: "FailedMount", Message: "Unable to attach or mount volumes: timed out waiting for the condition", LastTimestamp: at(3)},
		{Reason: "BackOff", Message: "Back-off restarting", LastTimestamp: at(4)},
	}
	if e := volumeEvent(events); e == nil || e.Message != "MountVolume.SetUp failed: newer" {
		t.Errorf("event = %+v, want the newest driver error", e)
	}
	if e := volumeEvent(events[3:]); e == nil || !strings.HasPrefix(e.Message, "Unable to attach") {
		t.Errorf("event = %+v, want the summary when there is nothing else", e)
	}
	if e := volumeEvent(events[4:]); e != nil {
		t.Errorf("event = %+v, want none", e)
	}
	series := []corev1.Event{
		{Reason: "FailedMount", Message: "a", LastTimestamp: at(5)},
		{Reason: "FailedMount", Message: "b", LastTimestamp: at(1), Series: &corev1.EventSeries{LastObservedTime: metav1.NewMicroTime(at(9).Time)}},
	}
	if e := volumeEvent(series); e == nil || e.Message != "b" {
		t.Errorf("event = %+v, want the series seen last", e)
	}
}

func TestShortenFoldsAndCuts(t *testing.T) {
	if got := shorten("mount failed\nOutput:  busy", 100); got != "mount failed Output: busy" {
		t.Errorf("shorten = %q", got)
	}
	if got := shorten("abcdef", 4); got != "abc…" {
		t.Errorf("shorten = %q", got)
	}
}
