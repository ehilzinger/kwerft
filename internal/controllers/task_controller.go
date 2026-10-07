// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// LabelTask selects the pods (and Job) of one Task.
	LabelTask = "kwerft.dev/task"
	// LabelSchedule marks the Tasks a Schedule started, and their pods.
	LabelSchedule = "kwerft.dev/schedule"
	// LabelAsApp gives the pods of a Task with fromApp that App's network
	// identity: whatever allows the App to connect allows them too.
	LabelAsApp = "kwerft.dev/as-app"

	// AnnotationRestartRequestedBy names the Task that asked for a restart
	// through kwerftv1.AnnotationRestartedAt (the console's Restart button uses
	// the same annotation, so there is one restart mechanism).
	AnnotationRestartRequestedBy = "kwerft.dev/restart-requested-by"

	// ConditionAppsRestarted is True once onSuccess.restart was carried out.
	ConditionAppsRestarted = "AppsRestarted"

	// restartFieldOwner applies only the restart annotations on Apps.
	restartFieldOwner = "kwerft-restart"
)

// TaskReconciler runs a Task as a Job (restartPolicy Never, the kwerft-batch
// priority class, a deny-ingress CiliumNetworkPolicy) and reports phase,
// times, the pod and how it ended (exit code, and the reason with the memory
// limit when it ran out of memory). Once the Task succeeded it rolls out the Apps in
// onSuccess.restart. A finished Task that no Schedule keeps is deleted after
// ttlSecondsAfterFinished, together with its Job and pods.
type TaskReconciler struct {
	client.Client
	// APIReader reads pods and their events (and confirms a missing Job)
	// from the API server, so Kwerft does not cache every pod in the
	// cluster.
	APIReader client.Reader
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

func (r *TaskReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var task kwerftv1.Task
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !task.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // Job, pods and policy are garbage-collected
	}
	orig := task.DeepCopy()

	err := r.reconcile(ctx, &task)
	if err != nil {
		setReady(&task.Status.Conditions, task.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
		if isTerminal(err) && !task.Status.Phase.Finished() && task.Status.Job == "" {
			task.Status.Phase = kwerftv1.TaskFailed // it cannot start; say so
		}
	}
	task.Status.ObservedGeneration = task.Generation
	if !equality.Semantic.DeepEqual(orig.Status, task.Status) {
		if perr := patchStatus(ctx, r.Client, &task, orig); perr != nil {
			return ctrl.Result{}, perr
		}
	}
	if err != nil {
		if isTerminal(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if task.Status.Phase == kwerftv1.TaskPending && task.Status.Job != "" {
		// The pod may wait for a disk; that only shows in events.
		return ctrl.Result{RequeueAfter: volumeWaitRequeue}, nil
	}
	return r.expire(ctx, &task)
}

func (r *TaskReconciler) reconcile(ctx context.Context, task *kwerftv1.Task) error {
	if task.Status.Phase == "" {
		task.Status.Phase = kwerftv1.TaskPending
	}
	if by := task.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" && !task.Status.Phase.Finished() {
		return r.cancel(ctx, task, by)
	}
	if !task.Status.Phase.Finished() {
		job, wait, err := r.ensureJob(ctx, task)
		switch {
		case err != nil:
			return err
		case wait != nil:
			setReady(&task.Status.Conditions, task.Generation, wait.status, wait.reason, wait.message)
			return nil
		case job != nil:
			if err := r.observe(ctx, task, job); err != nil {
				return err
			}
		}
	}
	if task.Status.Phase == kwerftv1.TaskSucceeded {
		return r.restartApps(ctx, task)
	}
	return nil
}

// cancel stops an unfinished run: the Job goes (its pods with it) and the
// Task becomes final, Failed with reason Cancelled, so neither it nor its
// Schedule starts it again. A Task without a Job yet never gets one.
func (r *TaskReconciler) cancel(ctx context.Context, task *kwerftv1.Task, by string) error {
	job := &batchv1.Job{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Name}, job); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case metav1.IsControlledBy(job, task):
		if err := client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
			return fmt.Errorf("delete job: %w", err)
		}
	}
	task.Status.Phase = kwerftv1.TaskFailed
	task.Status.CompletionTime = ptrTime(r.now())
	setReady(&task.Status.Conditions, task.Generation, metav1.ConditionFalse, "Cancelled", "Cancelled by "+by)
	return nil
}

// ensureJob returns the Task's Job, creating it (and its network policy) on
// the first run. The Job is created once and never re-applied, so changes to
// the App afterwards cannot touch a running Task.
func (r *TaskReconciler) ensureJob(ctx context.Context, task *kwerftv1.Task) (*batchv1.Job, *readiness, error) {
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: task.Namespace, Name: task.Name}
	err := r.Get(ctx, key, job)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, key, job) // the cache may lag right after creation
	}
	switch {
	case err == nil:
		if !metav1.IsControlledBy(job, task) {
			return nil, nil, terminalf("JobConflict", "a Job named %q already exists and does not belong to this Task", task.Name)
		}
		return job, nil, nil
	case !apierrors.IsNotFound(err):
		return nil, nil, err
	case task.Status.Job != "":
		task.Status.Phase = kwerftv1.TaskFailed
		task.Status.CompletionTime = ptrTime(r.now())
		return nil, &readiness{metav1.ConditionFalse, "JobDeleted", "The Job " + task.Status.Job + " was deleted before it finished"}, nil
	}

	project, err := projectOf(ctx, r.Client, task.Namespace)
	if err != nil {
		return nil, nil, err
	}
	var app *kwerftv1.App
	if name := task.Spec.FromApp; name != "" {
		app = &kwerftv1.App{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, app); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, &readiness{metav1.ConditionFalse, "AppNotFound", "Waiting for App " + name}, nil
			}
			return nil, nil, err
		}
	}
	var appImage *resolvedImage
	if app != nil {
		if appImage, err = resolveImage(ctx, r.Client, app); err != nil {
			return nil, nil, err
		}
	}
	run, wait := resolveTask(task, app, appImage, project)
	if wait != nil {
		return nil, wait, nil
	}
	missing, err := missingVolumes(ctx, r.Client, task.Namespace, run.volumes)
	if err != nil {
		return nil, nil, err
	}
	if len(missing) > 0 {
		return nil, &readiness{metav1.ConditionFalse, "VolumeNotFound", "Waiting for Volume " + strings.Join(missing, ", ")}, nil
	}

	// The policy first, so the pod never runs unrestricted. Never take over
	// a policy someone else made.
	existing := &metav1.PartialObjectMetadata{}
	existing.SetGroupVersionKind(CiliumNetworkPolicyGVK)
	switch err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: taskPolicyName(task.Name)}, existing); {
	case apierrors.IsNotFound(err):
	case meta.IsNoMatchError(err):
		return nil, nil, terminalf("CiliumMissing", "Cilium's CiliumNetworkPolicy is not installed in this cluster; re-run the installer")
	case err != nil:
		return nil, nil, err
	case !metav1.IsControlledBy(existing, task):
		return nil, nil, terminalf("PolicyConflict", "a CiliumNetworkPolicy named %q already exists and does not belong to this Task", existing.Name)
	}
	if err := apply(ctx, r.Client, client.ApplyConfigurationFromUnstructured(run.ciliumPolicy())); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil, terminalf("CiliumMissing", "Cilium's CiliumNetworkPolicy is not installed in this cluster; re-run the installer")
		}
		return nil, nil, fmt.Errorf("apply network policy: %w", err)
	}
	// Through v0.6.0-rc.7 a Task's policy was a Kubernetes NetworkPolicy of
	// the same name, which cannot name the nodes. A Task that wrote one and
	// never got its Job (the Job was rejected) would keep it.
	if err := deleteIfControlledBy(ctx, r.Client, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: taskPolicyName(task.Name), Namespace: task.Namespace}}, task); err != nil {
		return nil, nil, err
	}
	if err := apply(ctx, r.Client, run.job()); err != nil {
		if apierrors.IsInvalid(err) {
			return nil, nil, terminalf("InvalidJob", "the Job was rejected: %v", err)
		}
		return nil, nil, fmt.Errorf("apply job: %w", err)
	}
	task.Status.Job = task.Name
	task.Status.Image = run.image
	setReady(&task.Status.Conditions, task.Generation, metav1.ConditionFalse, "Pending", "Job created")
	return nil, nil, nil // the Job's own events bring us back
}

// observe copies the Job's and its latest pod's state into the Task status.
func (r *TaskReconciler) observe(ctx context.Context, task *kwerftv1.Task, job *batchv1.Job) error {
	st := &task.Status
	st.Job = job.Name
	if job.Status.StartTime != nil {
		st.StartTime = job.Status.StartTime
	}

	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods, client.InNamespace(task.Namespace), client.MatchingLabels{LabelTask: task.Name}); err != nil {
		return err
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if pod == nil || pod.CreationTimestamp.Before(&p.CreationTimestamp) ||
			(pod.CreationTimestamp.Equal(&p.CreationTimestamp) && pod.Name < p.Name) {
			pod = p
		}
	}
	waiting := ""
	if pod != nil {
		st.Pod = pod.Name
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != "task" {
				continue
			}
			switch {
			case cs.State.Terminated != nil:
				st.ExitCode = &cs.State.Terminated.ExitCode
				st.TerminationReason = cs.State.Terminated.Reason
				st.MemoryLimit = nil
				if st.TerminationReason == reasonOOMKilled {
					st.MemoryLimit = memoryLimit(pod, cs.Name)
				}
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				waiting = cs.State.Waiting.Reason
				if cs.State.Waiting.Message != "" {
					waiting += ": " + cs.State.Waiting.Message
				}
			}
		}
	}

	if c := jobCondition(job, batchv1.JobComplete); c != nil {
		st.Phase = kwerftv1.TaskSucceeded
		st.CompletionTime = job.Status.CompletionTime
		if st.CompletionTime == nil {
			st.CompletionTime = &c.LastTransitionTime
		}
		setReady(&st.Conditions, task.Generation, metav1.ConditionTrue, "Succeeded", "Finished successfully")
		return nil
	}
	if c := jobCondition(job, batchv1.JobFailed); c != nil {
		st.Phase = kwerftv1.TaskFailed
		st.CompletionTime = &c.LastTransitionTime
		reason, msg := c.Reason, c.Message
		if reason == "" {
			reason = "Failed"
		}
		if msg == "" {
			msg = "The job failed"
		}
		if st.TerminationReason == reasonOOMKilled {
			// Exit code 137 alone reads like a crash; say what happened
			// and what to change.
			reason, msg = "OutOfMemory", "The run used more memory than its limit"
			if st.MemoryLimit != nil {
				msg += " (" + st.MemoryLimit.String() + ")"
			}
			msg += " and was stopped; give the Task a larger size"
		}
		if st.ExitCode != nil {
			msg = fmt.Sprintf("%s (exit code %d)", msg, *st.ExitCode)
		}
		setReady(&st.Conditions, task.Generation, metav1.ConditionFalse, reason, msg)
		return nil
	}
	if pod != nil && pod.Status.Phase == corev1.PodRunning {
		st.Phase = kwerftv1.TaskRunning
		setReady(&st.Conditions, task.Generation, metav1.ConditionFalse, "Running", "Pod "+pod.Name+" is running")
		return nil
	}
	st.Phase = kwerftv1.TaskPending
	msg := "Waiting for the pod to start"
	if waiting != "" {
		msg = waiting
	}
	// The pod waits in ContainerCreating for a Secret it mounts.
	if missing := missingSecrets(ctx, r.APIReader, task.Namespace, job.Spec.Template.Spec.Volumes); len(missing) > 0 {
		msg = "Waiting for the pod to start: " + secretsMissing(missing)
	} else if pod != nil {
		if w := volumeWait(ctx, r.APIReader, []corev1.Pod{*pod}); w != "" {
			msg = "Waiting for the pod to start: " + w
		}
	}
	setReady(&st.Conditions, task.Generation, metav1.ConditionFalse, "Pending", msg)
	return nil
}

// reasonOOMKilled is the reason Kubernetes gives a container the kernel
// stopped for using more memory than its limit.
const reasonOOMKilled = "OOMKilled"

// memoryLimit is the memory limit of pod's container, nil without one.
func memoryLimit(pod *corev1.Pod, container string) *resource.Quantity {
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && c.Name == container {
			return &q
		}
	}
	return nil
}

func jobCondition(job *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		if c := &job.Status.Conditions[i]; c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

// restartApps rolls out the Apps in onSuccess.restart, once per Task: it sets
// the restart annotation to the Task's completion time, so repeating it after
// a failed status write changes nothing.
func (r *TaskReconciler) restartApps(ctx context.Context, task *kwerftv1.Task) error {
	if task.Spec.OnSuccess == nil || len(task.Spec.OnSuccess.Restart) == 0 ||
		meta.IsStatusConditionTrue(task.Status.Conditions, ConditionAppsRestarted) {
		return nil
	}
	at := task.Status.CompletionTime
	if at == nil {
		at = ptrTime(r.now())
		task.Status.CompletionTime = at
	}
	var restarted, missing []string
	for _, name := range task.Spec.OnSuccess.Restart {
		var app kwerftv1.App
		if err := r.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, &app); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, name)
				continue
			}
			return err
		}
		// A merge patch, not an apply: an apply would recreate an App deleted
		// in the meantime as an empty one.
		patch := client.MergeFrom(app.DeepCopy())
		if app.Annotations == nil {
			app.Annotations = map[string]string{}
		}
		app.Annotations[kwerftv1.AnnotationRestartedAt] = at.UTC().Format(time.RFC3339)
		app.Annotations[AnnotationRestartRequestedBy] = "Task/" + task.Name
		if err := r.Patch(ctx, &app, patch, client.FieldOwner(restartFieldOwner)); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, name) // deleted meanwhile
				continue
			}
			return fmt.Errorf("restart App %s: %w", name, err)
		}
		restarted = append(restarted, name)
	}
	msg := "Rolled out " + strings.Join(restarted, ", ")
	if len(restarted) == 0 {
		msg = "Nothing rolled out"
	}
	if len(missing) > 0 {
		msg += "; not found: " + strings.Join(missing, ", ")
	}
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:               ConditionAppsRestarted,
		Status:             metav1.ConditionTrue,
		Reason:             "Restarted",
		Message:            msg,
		ObservedGeneration: task.Generation,
	})
	return nil
}

// expire deletes a finished Task once its TTL has passed, unless a Schedule
// controls it (the Schedule's history decides then).
func (r *TaskReconciler) expire(ctx context.Context, task *kwerftv1.Task) (ctrl.Result, error) {
	if !task.Status.Phase.Finished() || task.Status.CompletionTime == nil {
		return ctrl.Result{}, nil
	}
	if owner := metav1.GetControllerOf(task); owner != nil && owner.Kind == "Schedule" {
		return ctrl.Result{}, nil
	}
	ttl := time.Duration(defaultTaskTTL) * time.Second
	if task.Spec.TTLSecondsAfterFinished != nil {
		ttl = time.Duration(*task.Spec.TTLSecondsAfterFinished) * time.Second
	}
	left := task.Status.CompletionTime.Add(ttl).Sub(r.now())
	if left > 0 {
		return ctrl.Result{RequeueAfter: left}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, task, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

func ptrTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

// waitingTasks enqueues the Tasks in the object's namespace that have not
// started their Job yet: a new App, a finished build or a new Volume may be
// what they wait for.
func (r *TaskReconciler) waitingTasks(ctx context.Context, obj client.Object) []reconcile.Request {
	var list kwerftv1.TaskList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, t := range list.Items {
		if t.Status.Job == "" && !t.Status.Phase.Finished() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&t)})
		}
	}
	return reqs
}

func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	waiting := handler.EnqueueRequestsFromMapFunc(r.waitingTasks)
	policy := &metav1.PartialObjectMetadata{}
	policy.SetGroupVersionKind(CiliumNetworkPolicyGVK)
	return ctrl.NewControllerManagedBy(mgr).
		// Status writes do not bump the generation, so they do not re-trigger;
		// an annotation carries a cancel request.
		For(&kwerftv1.Task{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Owns(&batchv1.Job{}).
		Owns(policy, builder.OnlyMetadata).
		Watches(&kwerftv1.App{}, waiting).
		Watches(&kwerftv1.Volume{}, waiting).
		// A successful build gives a Task from a Git app its image.
		Watches(&kwerftv1.Build{}, waiting).
		Named("task").
		Complete(r)
}
