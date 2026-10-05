package controllers

import (
	"math"
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// BatchPriorityClass ranks Task pods below Apps (shipped by the chart),
	// so under memory pressure a job goes before a service.
	BatchPriorityClass = "kwerft-batch"

	defaultTaskTTL = 7 * 24 * 60 * 60 // seconds
)

// taskRun is a Task resolved against the App it starts from: everything the
// Job needs, decided once when the Task starts.
type taskRun struct {
	task       *kwerftv1.Task
	project    string
	image      string
	pullSecret string
	command    []string
	env        []corev1.EnvVar
	size       string
	resources  *corev1.ResourceRequirements
	egress     string
	volumes    []kwerftv1.AppVolume
	asApp      string // FromApp: the pods connect with that App's identity
	owner      *metav1ac.OwnerReferenceApplyConfiguration
}

// resolveTask merges the Task over its App (nil without FromApp). It returns
// a reason and message instead when the Task has to wait.
// appImage is the App's resolved image (resolveImage), nil while it has none.
func resolveTask(task *kwerftv1.Task, app *kwerftv1.App, appImage *resolvedImage, project string) (*taskRun, *readiness) {
	s := task.Spec
	run := &taskRun{
		task:    task,
		project: project,
		owner:   controllerRef(task, kwerftv1.GroupVersion.WithKind("Task")),
		command: s.Command,
		size:    s.Size,
		egress:  s.Egress,
	}
	if s.Size != "" {
		run.resources = s.Resources
	}

	if app != nil {
		run.asApp = app.Name
		if len(run.command) == 0 {
			run.command = app.Spec.Command
		}
		if run.size == "" {
			run.size, run.resources = app.Spec.Size, app.Spec.Resources
		}
		if run.egress == "" {
			run.egress = app.Spec.Egress
		}
		run.env = slices.Clone(app.Spec.Env)
		for _, v := range app.Spec.Volumes {
			if !v.OwnDisk() { // a replica's own disk cannot be shared
				run.volumes = append(run.volumes, v)
			}
		}
	}
	run.env = mergeEnv(mergeEnv(run.env, s.Env), s.EnvOverrides)
	for _, v := range s.Volumes {
		if i := slices.IndexFunc(run.volumes, func(o kwerftv1.AppVolume) bool { return o.Path == v.Path }); i >= 0 {
			run.volumes[i] = v
		} else {
			run.volumes = append(run.volumes, v)
		}
	}
	if run.egress == "" {
		run.egress = "https"
	}

	switch {
	case s.Source != nil && s.Source.Image != nil:
		run.image, run.pullSecret = s.Source.Image.Ref, s.Source.Image.PullSecret
	case app != nil:
		if appImage == nil {
			return nil, &readiness{metav1.ConditionFalse, "AwaitingBuild", "Waiting for the first successful build of App " + app.Name}
		}
		run.image = appImage.image
		if src := app.Spec.Source.Image; src != nil {
			run.pullSecret = src.PullSecret
		}
	default:
		return nil, &readiness{metav1.ConditionFalse, "NoSource", "The Task has no image to run"}
	}
	return run, nil
}

// mergeEnv returns base with over applied: same name replaces in place, new
// names are appended.
func mergeEnv(base, over []corev1.EnvVar) []corev1.EnvVar {
	out := slices.Clone(base)
	for _, e := range over {
		if i := slices.IndexFunc(out, func(o corev1.EnvVar) bool { return o.Name == e.Name }); i >= 0 {
			out[i] = e
		} else {
			out = append(out, e)
		}
	}
	return out
}

func (r *taskRun) labels() map[string]string {
	l := map[string]string{
		LabelTask:      r.task.Name,
		LabelProject:   r.project,
		LabelManagedBy: ManagedByKwerft,
	}
	if r.asApp != "" {
		l[LabelAsApp] = r.asApp
	}
	if s := r.task.Labels[LabelSchedule]; s != "" {
		l[LabelSchedule] = s
	}
	return l
}

func (r *taskRun) job() *batchv1ac.JobApplyConfiguration {
	pod := &podShape{
		container:  "task",
		image:      r.image,
		pullSecret: r.pullSecret,
		command:    r.command,
		env:        r.env,
		size:       r.size,
		resources:  r.resources,
		volumes:    r.volumes,
		labels:     r.labels(),
	}
	tmpl := pod.template()
	tmpl.Spec.
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithPriorityClassName(BatchPriorityClass)

	s := r.task.Spec
	retries, ttl := int32(0), int32(defaultTaskTTL)
	if s.Retries != nil {
		retries = *s.Retries
	}
	if s.TTLSecondsAfterFinished != nil {
		ttl = *s.TTLSecondsAfterFinished
	}
	spec := batchv1ac.JobSpec().
		WithBackoffLimit(retries).
		WithTTLSecondsAfterFinished(ttl).
		WithTemplate(tmpl)
	if s.Timeout != nil {
		spec.WithActiveDeadlineSeconds(max(1, int64(math.Ceil(s.Timeout.Seconds()))))
	}
	return batchv1ac.Job(r.task.Name, r.task.Namespace).
		WithLabels(r.labels()).
		WithOwnerReferences(r.owner).
		WithSpec(spec)
}

// taskPolicyName names a Task's NetworkPolicy. App policies carry the App's
// name, which has no dot when the App has a Service, so they do not collide.
func taskPolicyName(task string) string { return task + ".task" }

// networkPolicy closes the Task's pods to all inbound traffic and limits
// outbound traffic like an App's.
func (r *taskRun) networkPolicy() *networkingv1ac.NetworkPolicyApplyConfiguration {
	spec := networkingv1ac.NetworkPolicySpec().
		WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{LabelTask: r.task.Name})).
		WithPolicyTypes(networkingv1.PolicyTypeIngress)
	withEgress(spec, r.egress)
	return networkingv1ac.NetworkPolicy(taskPolicyName(r.task.Name), r.task.Namespace).
		WithLabels(r.labels()).
		WithOwnerReferences(r.owner).
		WithSpec(spec)
}
