package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func createTask(t *testing.T, ns, name string, spec kwerftv1.TaskSpec) *kwerftv1.Task {
	t.Helper()
	task := &kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

// waitForTask waits until check accepts the Task and returns it.
func waitForTask(t *testing.T, task *kwerftv1.Task, check func(*kwerftv1.Task) error) *kwerftv1.Task {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(task), task); err != nil {
			return err
		}
		return check(task)
	})
	return task
}

func taskPhase(want kwerftv1.TaskPhase) func(*kwerftv1.Task) error {
	return func(task *kwerftv1.Task) error {
		if task.Status.Phase != want {
			return fmt.Errorf("phase %q, want %q (conditions %+v)", task.Status.Phase, want, task.Status.Conditions)
		}
		return nil
	}
}

func taskReason(want string) func(*kwerftv1.Task) error {
	return func(task *kwerftv1.Task) error {
		reason, err := readyReason(task.Status.Conditions, task.Generation)
		if err != nil {
			return err
		}
		if reason != want {
			return fmt.Errorf("reason %q, want %q", reason, want)
		}
		return nil
	}
}

func waitForJob(t *testing.T, ns, name string) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	eventually(t, func() error { return k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, job) })
	return job
}

// finishJob plays the Job controller: it marks the Job complete or failed at at.
func finishJob(t *testing.T, ns, name string, succeeded bool, at time.Time) {
	t.Helper()
	ctx := context.Background()
	job := waitForJob(t, ns, name)
	start := metav1.NewTime(at.Add(-time.Minute))
	end := metav1.NewTime(at)
	job.Status.StartTime = &start
	if succeeded {
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &end
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: "CompletionsReached", LastTransitionTime: end},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "CompletionsReached", LastTransitionTime: end},
		}
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: end},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: end},
		}
	}
	if err := k8s.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
}

// createTaskPod plays the Job controller and kubelet: a pod of the Task in
// the given state.
func createTaskPod(t *testing.T, ns, task, name string, state corev1.ContainerState, phase corev1.PodPhase) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{LabelTask: task}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "task", Image: "busybox:1.37"}},
		},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{
		Phase:             phase,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "task", Image: "busybox:1.37", State: state}},
	}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// poke changes an annotation so the object's reconciler runs again.
func poke(t *testing.T, obj client.Object) {
	t.Helper()
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann["test.kwerft.dev/poke"] = time.Now().Format(time.RFC3339Nano)
	obj.SetAnnotations(ann)
	if err := k8s.Patch(context.Background(), obj, patch); err != nil {
		t.Fatal(err)
	}
}

func imageTask(ref string) kwerftv1.TaskSpec {
	return kwerftv1.TaskSpec{Source: &kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: ref}}}
}

func TestTaskRendersJob(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "jobs")
	createVolume(t, "jobs", "exports", "1Gi", "")
	spec := imageTask("ghcr.io/acme/importer:2.0")
	spec.Source.Image.PullSecret = "ghcr-acme"
	spec.Command = []string{"/importer", "--all"}
	spec.Env = []corev1.EnvVar{{Name: "MODE", Value: "full"}}
	spec.Volumes = []kwerftv1.AppVolume{{Path: "/exports", Volume: "exports"}}
	spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	spec.Retries = ptr.To[int32](2)
	spec.TTLSecondsAfterFinished = ptr.To[int32](3600)
	spec.Egress = "none"
	spec.Size = "large"
	task := createTask(t, "jobs", "import", spec)
	task = waitForTask(t, task, taskReason("Pending"))

	job := waitForJob(t, "jobs", "import")
	if !metav1.IsControlledBy(job, task) {
		t.Error("job is not controlled by the Task")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 1800 {
		t.Errorf("activeDeadlineSeconds = %v, want 1800", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 2 {
		t.Errorf("backoffLimit = %v, want 2", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 3600", job.Spec.TTLSecondsAfterFinished)
	}
	pod := job.Spec.Template
	if pod.Spec.PriorityClassName != BatchPriorityClass {
		t.Errorf("priorityClassName = %q", pod.Spec.PriorityClassName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q", pod.Spec.RestartPolicy)
	}
	if pod.Labels[LabelTask] != "import" || pod.Labels[LabelProject] != "jobs" || pod.Labels[LabelManagedBy] != ManagedByKwerft {
		t.Errorf("pod labels = %v", pod.Labels)
	}
	if _, ok := pod.Labels[LabelApp]; ok {
		t.Error("a Task pod must not carry the app label (the App's Service would select it)")
	}
	c := pod.Spec.Containers[0]
	if c.Name != "task" || c.Image != "ghcr.io/acme/importer:2.0" || strings.Join(c.Command, " ") != "/importer --all" {
		t.Errorf("container = %s %s %v", c.Name, c.Image, c.Command)
	}
	if got := c.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("memory limit = %s, want 2Gi (large)", got.String())
	}
	if len(c.Env) != 1 || c.Env[0].Name != "MODE" {
		t.Errorf("env = %+v", c.Env)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "exports" ||
		len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/exports" {
		t.Errorf("volumes = %+v, mounts = %+v", pod.Spec.Volumes, c.VolumeMounts)
	}
	if ps := pod.Spec.ImagePullSecrets; len(ps) != 1 || ps[0].Name != "ghcr-acme" {
		t.Errorf("imagePullSecrets = %+v", ps)
	}

	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "jobs", Name: "import.task"}, &np); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&np, task) || np.Spec.PodSelector.MatchLabels[LabelTask] != "import" {
		t.Errorf("policy owner/selector = %v %v", np.OwnerReferences, np.Spec.PodSelector)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("ingress rules = %+v, want none", np.Spec.Ingress)
	}
	if len(np.Spec.PolicyTypes) != 2 || len(np.Spec.Egress) != 2 {
		t.Errorf("policyTypes %v, egress rules %d; want Ingress+Egress and 2 (dns, cluster)", np.Spec.PolicyTypes, len(np.Spec.Egress))
	}

	if task.Status.Phase != kwerftv1.TaskPending || task.Status.Job != "import" || task.Status.Image != "ghcr.io/acme/importer:2.0" {
		t.Errorf("status = %+v", task.Status)
	}
}

func TestTaskStatusFollowsJobAndPod(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "runs")
	task := createTask(t, "runs", "migrate", imageTask("busybox:1.37"))
	waitForJob(t, "runs", "migrate")

	createTaskPod(t, "runs", "migrate", "migrate-abcde",
		corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull failed"}}, corev1.PodPending)
	// Pod changes reach the Task through the Job; nudge it like the Job controller would.
	poke(t, waitForJob(t, "runs", "migrate"))
	task = waitForTask(t, task, func(task *kwerftv1.Task) error {
		c := meta.FindStatusCondition(task.Status.Conditions, ConditionReady)
		if c == nil || !strings.HasPrefix(c.Message, "ImagePullBackOff") {
			return fmt.Errorf("ready = %+v, want the waiting reason", c)
		}
		return nil
	})
	if task.Status.Pod != "migrate-abcde" {
		t.Errorf("pod = %q", task.Status.Pod)
	}

	// A later pod (a retry) is running.
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has second resolution
	createTaskPod(t, "runs", "migrate", "migrate-fghij",
		corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}, corev1.PodRunning)
	poke(t, waitForJob(t, "runs", "migrate"))
	task = waitForTask(t, task, taskPhase(kwerftv1.TaskRunning))
	if task.Status.Pod != "migrate-fghij" {
		t.Errorf("pod = %q, want the latest", task.Status.Pod)
	}

	// It exits 3 and the Job gives up.
	pod := &corev1.Pod{}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "runs", Name: "migrate-fghij"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Reason: "Error"}}
	if err := k8s.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	finishJob(t, "runs", "migrate", false, time.Now())
	task = waitForTask(t, task, taskPhase(kwerftv1.TaskFailed))
	if task.Status.ExitCode == nil || *task.Status.ExitCode != 3 {
		t.Errorf("exitCode = %v, want 3", task.Status.ExitCode)
	}
	if task.Status.CompletionTime == nil || task.Status.StartTime == nil {
		t.Errorf("times = start %v completion %v", task.Status.StartTime, task.Status.CompletionTime)
	}
	if reason, _ := readyReason(task.Status.Conditions, task.Generation); reason != "BackoffLimitExceeded" {
		t.Errorf("reason = %q", reason)
	}
}

func TestTaskOnSuccessRestartsAppsOnce(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "restart")
	app := createApp(t, "restart", "server", kwerftv1.AppSpec{Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.29"}}})
	waitForApp(t, app, "Progressing")

	spec := imageTask("busybox:1.37")
	spec.OnSuccess = &kwerftv1.TaskOnSuccess{Restart: []string{"server", "ghost"}}
	task := createTask(t, "restart", "refresh", spec)
	waitForJob(t, "restart", "refresh")
	finishJob(t, "restart", "refresh", true, time.Now())

	task = waitForTask(t, task, func(task *kwerftv1.Task) error {
		if !meta.IsStatusConditionTrue(task.Status.Conditions, ConditionAppsRestarted) {
			return fmt.Errorf("not restarted yet: %+v", task.Status.Conditions)
		}
		return nil
	})
	if task.Status.Phase != kwerftv1.TaskSucceeded {
		t.Errorf("phase = %q", task.Status.Phase)
	}
	if c := meta.FindStatusCondition(task.Status.Conditions, ConditionAppsRestarted); !strings.Contains(c.Message, "server") || !strings.Contains(c.Message, "not found: ghost") {
		t.Errorf("AppsRestarted message = %q", c.Message)
	}

	var at string
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
			return err
		}
		at = app.Annotations[kwerftv1.AnnotationRestartedAt]
		var d appsv1.Deployment
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), &d); err != nil {
			return err
		}
		if at == "" || d.Spec.Template.Annotations[kwerftv1.AnnotationRestartedAt] != at {
			return fmt.Errorf("app annotation %q, pod template annotation %q", at, d.Spec.Template.Annotations[kwerftv1.AnnotationRestartedAt])
		}
		if d.Generation != 2 {
			return fmt.Errorf("deployment generation %d, want 2 (one rollout)", d.Generation)
		}
		return nil
	})
	if app.Annotations[AnnotationRestartRequestedBy] != "Task/refresh" {
		t.Errorf("restart-requested-by = %q", app.Annotations[AnnotationRestartRequestedBy])
	}

	// Reconciling the Task again must not restart again.
	poke(t, waitForJob(t, "restart", "refresh"))
	time.Sleep(time.Second)
	var d appsv1.Deployment
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), &d); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	if d.Generation != 2 || app.Annotations[kwerftv1.AnnotationRestartedAt] != at {
		t.Errorf("restarted again: deployment generation %d, annotation %q (was %q)", d.Generation, app.Annotations[kwerftv1.AnnotationRestartedAt], at)
	}
}

func TestTaskFromAppInheritsAndOverrides(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "inherit")
	createVolume(t, "inherit", "media", "1Gi", "")

	// The Task comes first and waits for its App.
	spec := kwerftv1.TaskSpec{
		FromApp:      "api",
		Env:          []corev1.EnvVar{{Name: "B", Value: "task"}},
		EnvOverrides: []corev1.EnvVar{{Name: "FORCE", Value: "1"}, {Name: "A", Value: "override"}},
	}
	task := createTask(t, "inherit", "api-now", spec)
	waitForTask(t, task, taskReason("AppNotFound"))

	createApp(t, "inherit", "api", kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "ghcr.io/acme/api:1.42.0", PullSecret: "ghcr-acme"}},
		Command: []string{"/api", "serve"},
		Env:     []corev1.EnvVar{{Name: "A", Value: "app"}, {Name: "B", Value: "app"}},
		Size:    "medium",
		Egress:  "all",
		Ports:   []kwerftv1.AppPort{{Container: 8080}},
		Volumes: []kwerftv1.AppVolume{
			{Path: "/media", Volume: "media"},
			{Path: "/cache", Size: resource.MustParse("1Gi")}, // a replica's own disk: not for the Task
		},
	})
	job := waitForJob(t, "inherit", "api-now")
	task = waitForTask(t, task, taskReason("Pending"))

	pod := job.Spec.Template
	c := pod.Spec.Containers[0]
	if c.Image != "ghcr.io/acme/api:1.42.0" || strings.Join(c.Command, " ") != "/api serve" {
		t.Errorf("container = %s %v", c.Image, c.Command)
	}
	var env []string
	for _, e := range c.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	if got := strings.Join(env, " "); got != "A=override B=task FORCE=1" {
		t.Errorf("env = %s, want A=override B=task FORCE=1", got)
	}
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("cpu request = %s, want 500m (the App's medium)", got.String())
	}
	if len(c.Ports) != 0 || c.ReadinessProbe != nil {
		t.Error("a Task has no ports or probes")
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/media" {
		t.Errorf("mounts = %+v, want only the shared Volume", c.VolumeMounts)
	}
	if ps := pod.Spec.ImagePullSecrets; len(ps) != 1 || ps[0].Name != "ghcr-acme" {
		t.Errorf("imagePullSecrets = %+v", ps)
	}
	if pod.Labels[LabelAsApp] != "api" {
		t.Errorf("pod labels = %v, want the App's network identity", pod.Labels)
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.TTLSecondsAfterFinished != defaultTaskTTL || job.Spec.ActiveDeadlineSeconds != nil {
		t.Errorf("defaults: backoffLimit %d ttl %d deadline %v", *job.Spec.BackoffLimit, *job.Spec.TTLSecondsAfterFinished, job.Spec.ActiveDeadlineSeconds)
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "inherit", Name: "api-now.task"}, &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want Ingress only (the App's egress: all)", np.Spec.PolicyTypes)
	}
	if task.Status.Image != "ghcr.io/acme/api:1.42.0" {
		t.Errorf("status.image = %q", task.Status.Image)
	}
}

func TestFinishedTaskExpires(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "expire")
	spec := imageTask("busybox:1.37")
	spec.TTLSecondsAfterFinished = ptr.To[int32](60)
	task := createTask(t, "expire", "once", spec)
	waitForJob(t, "expire", "once")
	finishJob(t, "expire", "once", true, time.Now().Add(-2*time.Minute))
	eventually(t, func() error {
		err := k8s.Get(context.Background(), client.ObjectKeyFromObject(task), &kwerftv1.Task{})
		if err == nil {
			return fmt.Errorf("task still there")
		}
		return client.IgnoreNotFound(err)
	})
}

func TestTaskCancelStopsTheRun(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "cancel")
	ctx := context.Background()
	task := createTask(t, "cancel", "long", imageTask("busybox:1.37"))
	waitForJob(t, "cancel", "long")
	jobGone := func() error {
		job := &batchv1.Job{}
		err := k8s.Get(ctx, client.ObjectKey{Namespace: "cancel", Name: "long"}, job)
		if err == nil && job.DeletionTimestamp.IsZero() {
			return fmt.Errorf("job still there")
		}
		return client.IgnoreNotFound(err) // or being deleted: no garbage collector in envtest
	}

	patch := client.MergeFrom(task.DeepCopy())
	task.Annotations = map[string]string{kwerftv1.AnnotationCancelRequested: "dev@example.com"}
	if err := k8s.Patch(ctx, task, patch); err != nil {
		t.Fatal(err)
	}
	task = waitForTask(t, task, taskPhase(kwerftv1.TaskFailed))
	c := meta.FindStatusCondition(task.Status.Conditions, ConditionReady)
	if c == nil || c.Reason != "Cancelled" || !strings.Contains(c.Message, "dev@example.com") {
		t.Errorf("ready = %+v, want Cancelled by the user", c)
	}
	if task.Status.CompletionTime == nil {
		t.Error("no completion time")
	}
	eventually(t, jobGone)

	// The cancelled Task is final: no new Job.
	poke(t, task)
	time.Sleep(500 * time.Millisecond)
	if err := jobGone(); err != nil {
		t.Errorf("after a cancel: %v", err)
	}
}
