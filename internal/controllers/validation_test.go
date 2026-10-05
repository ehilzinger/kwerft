package controllers

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// expectInvalid fails unless err is a validation error mentioning want.
func expectInvalid(t *testing.T, err error, want string) {
	t.Helper()
	if !apierrors.IsInvalid(err) {
		t.Errorf("expected a validation error (%q), got %v", want, err)
		return
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}
}

func TestCELRejectsInvalidSpecs(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := "default"
	image := kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx"}}
	app := func(name string, vols ...kwerftv1.AppVolume) *kwerftv1.App {
		return &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: kwerftv1.AppSpec{Source: image, Volumes: vols}}
	}
	task := func(name string, spec kwerftv1.TaskSpec) *kwerftv1.Task {
		return &kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
	}

	t.Run("app volume with both size and volume", func(t *testing.T) {
		err := k8s.Create(ctx, app("both-forms", kwerftv1.AppVolume{Path: "/d", Size: resource.MustParse("1Gi"), Volume: "data"}))
		expectInvalid(t, err, "set exactly one of size")
	})
	t.Run("app volume with neither", func(t *testing.T) {
		err := k8s.Create(ctx, app("no-form", kwerftv1.AppVolume{Path: "/d"}))
		expectInvalid(t, err, "set exactly one of size")
	})
	t.Run("class on a shared volume", func(t *testing.T) {
		err := k8s.Create(ctx, app("class-shared", kwerftv1.AppVolume{Path: "/d", Volume: "data", Class: "hcloud-volume"}))
		expectInvalid(t, err, "class applies to size")
	})
	t.Run("drain longer than five minutes", func(t *testing.T) {
		a := app("long-drain")
		a.Spec.DrainSeconds = ptr.To[int32](301)
		expectInvalid(t, k8s.Create(ctx, a), "drainSeconds")
	})
	t.Run("both app volume forms are accepted", func(t *testing.T) {
		a := app("mixed", kwerftv1.AppVolume{Path: "/own", Size: resource.MustParse("1Gi")}, kwerftv1.AppVolume{Path: "/shared", Volume: "data"})
		if err := k8s.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("project names reserved for the platform", func(t *testing.T) {
		// kubectl must not get around the console's check: a Project would
		// otherwise label a platform namespace and lock it down.
		names := append([]string{"kube-system", "kwerft-system", "kwerft-builds", "kwerft-observability", "kwerft-anything"}, kwerftv1.ReservedProjectNames...)
		for _, name := range names {
			if !kwerftv1.IsReservedProjectName(name) {
				t.Errorf("IsReservedProjectName(%q) = false", name)
			}
			expectInvalid(t, k8s.Create(ctx, &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}), "reserved for the platform")
		}
	})
	t.Run("project names that only look like platform ones", func(t *testing.T) {
		for _, name := range []string{"kwerft", "my-kwerft-app", "kubernetes", "defaults"} {
			if kwerftv1.IsReservedProjectName(name) {
				t.Errorf("IsReservedProjectName(%q) = true", name)
			}
			p := &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
			if err := k8s.Create(ctx, p); err != nil {
				t.Errorf("project %q: %v", name, err)
				continue
			}
			_ = k8s.Delete(ctx, p)
		}
	})

	t.Run("task without source or fromApp", func(t *testing.T) {
		expectInvalid(t, k8s.Create(ctx, task("nothing", kwerftv1.TaskSpec{})), "set source or fromApp")
	})
	t.Run("task with a git source", func(t *testing.T) {
		err := k8s.Create(ctx, task("git", kwerftv1.TaskSpec{Source: &kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: "https://example.com/x.git"}}}))
		expectInvalid(t, err, "a Task runs an image")
	})
	t.Run("task with a disk of its own", func(t *testing.T) {
		spec := imageTask("busybox")
		spec.Volumes = []kwerftv1.AppVolume{{Path: "/d", Size: resource.MustParse("1Gi")}}
		expectInvalid(t, k8s.Create(ctx, task("own-disk", spec)), "a Task can only mount shared Volumes")
	})
	t.Run("task with a zero timeout", func(t *testing.T) {
		spec := imageTask("busybox")
		spec.Timeout = &metav1.Duration{}
		expectInvalid(t, k8s.Create(ctx, task("no-time", spec)), "timeout must be at least 1s")
	})
	t.Run("task spec is immutable", func(t *testing.T) {
		tk := task("fixed", imageTask("busybox"))
		if err := k8s.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
		// A merge patch: the Task reconciler writes status meanwhile, which
		// would make an update conflict before validation even runs.
		orig := tk.DeepCopy()
		tk.Spec.Command = []string{"sh"}
		expectInvalid(t, k8s.Patch(ctx, tk, client.MergeFrom(orig)), "a Task cannot be changed")
	})

	t.Run("volume class is immutable and size only grows", func(t *testing.T) {
		vol := &kwerftv1.Volume{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "grow"},
			Spec:       kwerftv1.VolumeSpec{Size: resource.MustParse("2Gi")},
		}
		if err := k8s.Create(ctx, vol); err != nil {
			t.Fatal(err)
		}
		if vol.Spec.Class != "local-nvme" {
			t.Errorf("default class = %q", vol.Spec.Class)
		}
		// The reconciler adds its finalizer meanwhile: update the latest version.
		update := func(mutate func(*kwerftv1.Volume)) error {
			var err error
			for range 10 {
				latest := &kwerftv1.Volume{}
				if err = k8s.Get(ctx, client.ObjectKeyFromObject(vol), latest); err != nil {
					return err
				}
				mutate(latest)
				if err = k8s.Update(ctx, latest); !apierrors.IsConflict(err) {
					return err
				}
			}
			return err
		}
		expectInvalid(t, update(func(v *kwerftv1.Volume) { v.Spec.Class = "hcloud-volume" }), "class cannot be changed")
		expectInvalid(t, update(func(v *kwerftv1.Volume) { v.Spec.Size = resource.MustParse("1Gi") }), "size can only grow")
		if err := update(func(v *kwerftv1.Volume) { v.Spec.Size = resource.MustParse("3Gi") }); err != nil {
			t.Errorf("growing must be allowed: %v", err)
		}
	})
	t.Run("volume size must be positive", func(t *testing.T) {
		vol := &kwerftv1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "empty"}, Spec: kwerftv1.VolumeSpec{Size: resource.MustParse("0")}}
		expectInvalid(t, k8s.Create(ctx, vol), "size must be positive")
	})

	t.Run("schedule defaults", func(t *testing.T) {
		s := &kwerftv1.Schedule{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "defaults"},
			Spec:       kwerftv1.ScheduleSpec{Schedule: "@daily", Suspend: true, Task: imageTask("busybox")},
		}
		if err := k8s.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
			t.Fatal(err)
		}
		if s.Spec.Concurrency != kwerftv1.ConcurrencyForbid || s.Spec.StartingDeadline == nil || s.Spec.StartingDeadline.Duration.String() != "1h0m0s" ||
			s.Spec.History.Succeeded == nil || *s.Spec.History.Succeeded != 3 || s.Spec.History.Failed == nil || *s.Spec.History.Failed != 3 ||
			s.Spec.Task.Retries == nil || *s.Spec.Task.Retries != 0 || s.Spec.Task.TTLSecondsAfterFinished == nil || *s.Spec.Task.TTLSecondsAfterFinished != defaultTaskTTL {
			t.Errorf("defaults not applied: %+v", s.Spec)
		}
	})
	t.Run("schedule name too long", func(t *testing.T) {
		s := &kwerftv1.Schedule{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: strings.Repeat("s", 53)},
			Spec:       kwerftv1.ScheduleSpec{Schedule: "@daily", Task: imageTask("busybox")},
		}
		expectInvalid(t, k8s.Create(ctx, s), "at most 52 characters")
	})

	t.Run("a command whose program contains whitespace", func(t *testing.T) {
		// What a one-line "echo hi" typed into a one-argument-per-line field became.
		spec := imageTask("busybox")
		spec.Command = []string{"echo hi"}
		expectInvalid(t, k8s.Create(ctx, task("one-arg", spec)), "the program (first item) contains whitespace")
		a := app("one-arg")
		a.Spec.Command = []string{"sh -c date"}
		expectInvalid(t, k8s.Create(ctx, a), "the program (first item) contains whitespace")
	})
	t.Run("spaces in later arguments are fine", func(t *testing.T) {
		spec := imageTask("busybox")
		spec.Command = []string{"sh", "-c", "echo hi && date"}
		if err := k8s.Create(ctx, task("sh-c", spec)); err != nil {
			t.Fatal(err)
		}
	})
}
