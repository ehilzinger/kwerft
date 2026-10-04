package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // IANA time zones even in a minimal image

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// AnnotationScheduledAt on a Task is the time its Schedule was due.
	AnnotationScheduledAt = "kwerft.dev/scheduled-at"

	defaultStartingDeadline = time.Hour
	defaultHistory          = 3
	// maxMissedRuns bounds the walk over missed runs within the deadline.
	maxMissedRuns = 100_000
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ScheduleReconciler starts a Task whenever a Schedule is due. It schedules
// itself, requeueing until the next run, instead of rendering a CronJob: a
// CronJob can only create Jobs, and creating Tasks from a pod would need
// RBAC inside the project.
type ScheduleReconciler struct {
	client.Client
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

func (r *ScheduleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s kwerftv1.Schedule
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !s.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // its Tasks are garbage-collected
	}
	orig := s.DeepCopy()

	res, ready, err := r.reconcile(ctx, &s)
	switch {
	case err != nil:
		setReady(&s.Status.Conditions, s.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
	case ready != nil:
		setReady(&s.Status.Conditions, s.Generation, ready.status, ready.reason, ready.message)
	}
	s.Status.ObservedGeneration = s.Generation
	if !equality.Semantic.DeepEqual(orig.Status, s.Status) {
		if perr := r.Status().Patch(ctx, &s, client.MergeFrom(orig)); perr != nil {
			return ctrl.Result{}, perr
		}
	}
	if isTerminal(err) {
		return ctrl.Result{}, nil
	}
	return res, err
}

func (r *ScheduleReconciler) reconcile(ctx context.Context, s *kwerftv1.Schedule) (ctrl.Result, *readiness, error) {
	now := r.now()
	sched, loc, err := parseSchedule(s.Spec.Schedule, s.Spec.TimeZone)
	if err != nil {
		return ctrl.Result{}, nil, err
	}

	active, err := r.observeRuns(ctx, s)
	if err != nil {
		return ctrl.Result{}, nil, err
	}

	next := sched.Next(now.In(loc))
	if s.Spec.Suspend {
		s.Status.NextScheduleTime = nil
		return ctrl.Result{}, &readiness{metav1.ConditionTrue, "Suspended", "Suspended; no new runs start"}, nil
	}
	s.Status.NextScheduleTime = &metav1.Time{Time: next}
	requeue := ctrl.Result{RequeueAfter: next.Sub(now) + time.Second}
	scheduled := &readiness{metav1.ConditionTrue, "Scheduled", "Next run at " + next.Format(time.RFC3339)}

	due := r.dueRun(s, sched, loc, now)
	if due.IsZero() {
		return requeue, scheduled, nil
	}

	if len(active) > 0 {
		switch s.Spec.Concurrency {
		case kwerftv1.ConcurrencyAllow:
		case kwerftv1.ConcurrencyReplace:
			for _, t := range active {
				// This very run, started by an earlier reconcile whose status
				// write the cache has not caught up with yet: keep it.
				if t.Name == runName(s, due) {
					continue
				}
				if err := client.IgnoreNotFound(r.Delete(ctx, t, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
					return ctrl.Result{}, nil, err
				}
			}
			s.Status.Active = nil
		default: // Forbid: the finishing Task brings us back while the run is still due.
			deadline := due.Add(startingDeadline(s))
			wait := min(next.Sub(now), deadline.Sub(now)) + time.Second
			return ctrl.Result{RequeueAfter: wait}, &readiness{metav1.ConditionTrue, "WaitingForActiveRun",
				fmt.Sprintf("The run due at %s waits for %s to finish (until %s)",
					due.Format(time.RFC3339), strings.Join(s.Status.Active, ", "), deadline.Format(time.RFC3339))}, nil
		}
	}

	name, err := r.startRun(ctx, s, due)
	if err != nil {
		return ctrl.Result{}, nil, err
	}
	s.Status.LastScheduleTime = &metav1.Time{Time: due}
	if !slices.Contains(s.Status.Active, name) {
		s.Status.Active = append(s.Status.Active, name)
	}
	return requeue, scheduled, nil
}

// ParseSchedule reads a cron expression and time zone exactly as the Schedule
// reconciler does, so the API can reject a bad one before it is written and
// preview the next runs.
func ParseSchedule(spec, tz string) (cron.Schedule, *time.Location, error) {
	return parseSchedule(spec, tz)
}

// parseSchedule parses a cron expression in the given IANA time zone (empty:
// the server's).
func parseSchedule(spec, tz string) (cron.Schedule, *time.Location, error) {
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		return nil, nil, terminalf("InvalidSchedule", "set the time zone in timeZone, not in the schedule")
	}
	sched, err := cronParser.Parse(spec)
	if err != nil {
		return nil, nil, terminalf("InvalidSchedule", "invalid schedule %q: %v", spec, err)
	}
	loc := time.Local
	if tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, nil, terminalf("InvalidTimeZone", "unknown time zone %q", tz)
		}
	}
	return sched, loc, nil
}

func startingDeadline(s *kwerftv1.Schedule) time.Duration {
	if d := s.Spec.StartingDeadline; d != nil && d.Duration > 0 {
		return d.Duration
	}
	return defaultStartingDeadline
}

// dueRun returns the latest scheduled time that has passed, has not run yet
// and is still within the starting deadline; zero if there is none. Older
// missed runs are dropped: after downtime a schedule runs once, not once per
// missed slot.
func (r *ScheduleReconciler) dueRun(s *kwerftv1.Schedule, sched cron.Schedule, loc *time.Location, now time.Time) time.Time {
	from := s.CreationTimestamp.Time
	if last := s.Status.LastScheduleTime; last != nil && last.After(from) {
		from = last.Time
	}
	if earliest := now.Add(-startingDeadline(s)); earliest.After(from) {
		from = earliest
	}
	var due time.Time
	t := from.In(loc)
	for range maxMissedRuns {
		n := sched.Next(t)
		if n.IsZero() || n.After(now) {
			break
		}
		due, t = n, n
	}
	return due
}

// observeRuns reads the Schedule's Tasks into its status (active runs, last
// success and failure), prunes finished ones beyond the history limits and
// returns the active ones.
func (r *ScheduleReconciler) observeRuns(ctx context.Context, s *kwerftv1.Schedule) ([]*kwerftv1.Task, error) {
	var list kwerftv1.TaskList
	if err := r.List(ctx, &list, client.InNamespace(s.Namespace), client.MatchingLabels{LabelSchedule: s.Name}); err != nil {
		return nil, err
	}
	var active, succeeded, failed []*kwerftv1.Task
	for i := range list.Items {
		t := &list.Items[i]
		if !metav1.IsControlledBy(t, s) || !t.DeletionTimestamp.IsZero() {
			continue
		}
		switch t.Status.Phase {
		case kwerftv1.TaskSucceeded:
			succeeded = append(succeeded, t)
			s.Status.LastSuccessTime = later(s.Status.LastSuccessTime, finishedAt(t))
		case kwerftv1.TaskFailed:
			failed = append(failed, t)
			s.Status.LastFailureTime = later(s.Status.LastFailureTime, finishedAt(t))
		default:
			active = append(active, t)
		}
	}
	s.Status.Active = nil
	for _, t := range active {
		s.Status.Active = append(s.Status.Active, t.Name)
	}
	slices.Sort(s.Status.Active)

	keepSucceeded, keepFailed := int32(defaultHistory), int32(defaultHistory)
	if h := s.Spec.History.Succeeded; h != nil {
		keepSucceeded = *h
	}
	if h := s.Spec.History.Failed; h != nil {
		keepFailed = *h
	}
	for _, prune := range []struct {
		tasks []*kwerftv1.Task
		keep  int
	}{{succeeded, int(keepSucceeded)}, {failed, int(keepFailed)}} {
		if len(prune.tasks) <= prune.keep {
			continue
		}
		// Newest first.
		slices.SortFunc(prune.tasks, func(a, b *kwerftv1.Task) int { return finishedAt(b).Compare(finishedAt(a).Time) })
		for _, t := range prune.tasks[prune.keep:] {
			if err := client.IgnoreNotFound(r.Delete(ctx, t, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
				return nil, err
			}
		}
	}
	return active, nil
}

func finishedAt(t *kwerftv1.Task) *metav1.Time {
	if t.Status.CompletionTime != nil {
		return t.Status.CompletionTime
	}
	return &t.CreationTimestamp
}

func later(a, b *metav1.Time) *metav1.Time {
	if a == nil || (b != nil && b.After(a.Time)) {
		return b
	}
	return a
}

// runName is deterministic per due time, so a retried reconcile cannot start
// the same run twice.
func runName(s *kwerftv1.Schedule, due time.Time) string {
	return fmt.Sprintf("%s-%d", s.Name, due.Unix()/60)
}

// startRun creates the Task for the run due at due, with server-side apply.
func (r *ScheduleReconciler) startRun(ctx context.Context, s *kwerftv1.Schedule, due time.Time) (string, error) {
	name := runName(s, due)
	var existing kwerftv1.Task
	switch err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, &existing); {
	case err == nil:
		if !metav1.IsControlledBy(&existing, s) {
			return "", terminalf("TaskConflict", "a Task named %q already exists and does not belong to this Schedule", name)
		}
		return name, nil // already started
	case !apierrors.IsNotFound(err):
		return "", err
	}

	task := &kwerftv1.Task{
		TypeMeta: metav1.TypeMeta{APIVersion: kwerftv1.GroupVersion.String(), Kind: "Task"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   s.Namespace,
			Labels:      map[string]string{LabelSchedule: s.Name, LabelManagedBy: ManagedByKwerft},
			Annotations: map[string]string{AnnotationScheduledAt: due.UTC().Format(time.RFC3339)},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         kwerftv1.GroupVersion.String(),
				Kind:               "Schedule",
				Name:               s.Name,
				UID:                s.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: *s.Spec.Task.DeepCopy(),
	}
	// Through encoding/json, which honours omitzero (AppVolume.size); the
	// reflection-based unstructured converter would write size: "0".
	raw, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	delete(obj, "status")
	unstructured.RemoveNestedField(obj, "metadata", "creationTimestamp")
	if err := apply(ctx, r.Client, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: obj})); err != nil {
		if apierrors.IsInvalid(err) {
			return "", terminalf("InvalidTask", "the Task for the run was rejected: %v", err)
		}
		return "", fmt.Errorf("start run: %w", err)
	}
	return name, nil
}

func (r *ScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Status writes neither bump the generation nor change annotations.
		For(&kwerftv1.Schedule{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Owns(&kwerftv1.Task{}).
		Named("schedule").
		Complete(r)
}
