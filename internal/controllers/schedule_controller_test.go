package controllers

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// --- unit tests: no API server -------------------------------------------

func TestParseSchedule(t *testing.T) {
	for _, tc := range []struct {
		spec, tz, reason string
	}{
		{"30 3 * * *", "Europe/Berlin", ""},
		{"@hourly", "", ""},
		{"@every 2h", "UTC", ""},
		{"61 * * * *", "", "InvalidSchedule"},
		{"* * * *", "", "InvalidSchedule"},
		{"CRON_TZ=UTC 0 3 * * *", "", "InvalidSchedule"},
		{"0 3 * * *", "Mars/Olympus", "InvalidTimeZone"},
	} {
		_, _, err := parseSchedule(tc.spec, tc.tz)
		switch {
		case tc.reason == "" && err != nil:
			t.Errorf("%q in %q: %v", tc.spec, tc.tz, err)
		case tc.reason != "" && (err == nil || reasonOf(err) != tc.reason):
			t.Errorf("%q in %q: got %v, want %s", tc.spec, tc.tz, err, tc.reason)
		}
	}
}

func TestDueRun(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, berlin)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		name     string
		spec     string
		created  string
		last     string // lastScheduleTime, optional
		now      string
		deadline time.Duration
		want     string // empty: nothing due
	}{
		{"not yet due", "0 3 * * *", "2026-10-01 12:00", "", "2026-10-02 02:59", time.Hour, ""},
		{"due in the schedule's time zone", "0 3 * * *", "2026-10-01 12:00", "", "2026-10-02 03:10", time.Hour, "2026-10-02 03:00"},
		{"already ran", "0 3 * * *", "2026-10-01 12:00", "2026-10-02 03:00", "2026-10-02 03:10", time.Hour, ""},
		{"missed beyond the deadline", "0 3 * * *", "2026-10-01 12:00", "", "2026-10-02 04:30", time.Hour, ""},
		{"after downtime only the latest missed run", "0 * * * *", "2026-10-01 12:00", "2026-10-02 01:00", "2026-10-02 04:30", 2 * time.Hour, "2026-10-02 04:00"},
		{"not before creation", "*/5 * * * *", "2026-10-02 10:01", "", "2026-10-02 10:03", time.Hour, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sched, _, err := parseSchedule(tc.spec, "Europe/Berlin")
			if err != nil {
				t.Fatal(err)
			}
			s := &kwerftv1.Schedule{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(at(tc.created))},
				Spec:       kwerftv1.ScheduleSpec{StartingDeadline: &metav1.Duration{Duration: tc.deadline}},
			}
			if tc.last != "" {
				s.Status.LastScheduleTime = &metav1.Time{Time: at(tc.last)}
			}
			got := (&ScheduleReconciler{}).dueRun(s, sched, berlin, at(tc.now).UTC())
			switch {
			case tc.want == "" && !got.IsZero():
				t.Errorf("due %v, want nothing", got)
			case tc.want != "" && !got.Equal(at(tc.want)):
				t.Errorf("due %v, want %v", got, at(tc.want))
			}
		})
	}
}

// --- integration tests ---------------------------------------------------

func createSchedule(t *testing.T, ns, name string, spec kwerftv1.ScheduleSpec) *kwerftv1.Schedule {
	t.Helper()
	s := &kwerftv1.Schedule{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

// scheduleTasks returns the Tasks of a Schedule, sorted by name (= due time).
func scheduleTasks(t *testing.T, s *kwerftv1.Schedule) []kwerftv1.Task {
	t.Helper()
	var list kwerftv1.TaskList
	if err := k8s.List(context.Background(), &list, client.InNamespace(s.Namespace), client.MatchingLabels{LabelSchedule: s.Name}); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(list.Items, func(a, b kwerftv1.Task) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	return list.Items
}

func waitForTaskCount(t *testing.T, s *kwerftv1.Schedule, n int) []kwerftv1.Task {
	t.Helper()
	var tasks []kwerftv1.Task
	eventually(t, func() error {
		if tasks = scheduleTasks(t, s); len(tasks) != n {
			return fmt.Errorf("%d tasks, want %d", len(tasks), n)
		}
		return nil
	})
	return tasks
}

func waitForSchedule(t *testing.T, s *kwerftv1.Schedule, check func(*kwerftv1.Schedule) error) *kwerftv1.Schedule {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(s), s); err != nil {
			return err
		}
		return check(s)
	})
	return s
}

func scheduleReason(want string) func(*kwerftv1.Schedule) error {
	return func(s *kwerftv1.Schedule) error {
		reason, err := readyReason(s.Status.Conditions, s.Generation)
		if err != nil {
			return err
		}
		if reason != want {
			return fmt.Errorf("reason %q, want %q (%+v)", reason, want, meta.FindStatusCondition(s.Status.Conditions, ConditionReady))
		}
		return nil
	}
}

func everyFiveMinutes() kwerftv1.ScheduleSpec {
	spec := kwerftv1.ScheduleSpec{Schedule: "*/5 * * * *", TimeZone: "Europe/Berlin", Task: imageTask("busybox:1.37")}
	spec.Task.Command = []string{"echo", "hello"}
	return spec
}

func TestScheduleStartsTaskWhenDue(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "cron")
	testClock.Reset()
	s := createSchedule(t, "cron", "report", everyFiveMinutes())
	s = waitForSchedule(t, s, scheduleReason("Scheduled"))
	if len(scheduleTasks(t, s)) != 0 {
		t.Fatal("nothing is due right after creation")
	}
	if s.Status.NextScheduleTime == nil {
		t.Fatal("nextScheduleTime not set")
	}

	testClock.Advance(6 * time.Minute)
	poke(t, s)
	tasks := waitForTaskCount(t, s, 1)
	task := tasks[0]
	if !metav1.IsControlledBy(&task, s) {
		t.Error("the run is not controlled by its Schedule")
	}
	if task.Annotations[AnnotationScheduledAt] == "" || task.Spec.Source.Image.Ref != "busybox:1.37" || len(task.Spec.Command) != 2 {
		t.Errorf("task = %+v / %+v", task.ObjectMeta, task.Spec)
	}
	s = waitForSchedule(t, s, func(s *kwerftv1.Schedule) error {
		if s.Status.LastScheduleTime == nil || !slices.Equal(s.Status.Active, []string{task.Name}) {
			return fmt.Errorf("status = %+v", s.Status)
		}
		return nil
	})
	due, _ := time.Parse(time.RFC3339, task.Annotations[AnnotationScheduledAt])
	if !s.Status.LastScheduleTime.Time.Equal(due) || due.Minute()%5 != 0 || task.Name != runName(s, due) {
		t.Errorf("lastScheduleTime %v, due %v, name %s", s.Status.LastScheduleTime, due, task.Name)
	}
	if !s.Status.NextScheduleTime.After(due) {
		t.Errorf("nextScheduleTime %v is not after the run at %v", s.Status.NextScheduleTime, due)
	}
	// The Task reconciler takes over: each run is a normal Task with a Job.
	waitForJob(t, "cron", task.Name)
}

func TestScheduleSuspendStartsNothing(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "paused")
	testClock.Reset()
	spec := everyFiveMinutes()
	spec.Suspend = true
	s := createSchedule(t, "paused", "report", spec)
	testClock.Advance(6 * time.Minute)
	poke(t, s)
	s = waitForSchedule(t, s, scheduleReason("Suspended"))
	time.Sleep(500 * time.Millisecond)
	if n := len(scheduleTasks(t, s)); n != 0 {
		t.Errorf("%d tasks while suspended", n)
	}
	if s.Status.NextScheduleTime != nil {
		t.Errorf("nextScheduleTime = %v while suspended", s.Status.NextScheduleTime)
	}
}

func TestScheduleForbidWaitsForActiveRun(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "forbid")
	testClock.Reset()
	s := createSchedule(t, "forbid", "sync", everyFiveMinutes())
	testClock.Advance(6 * time.Minute)
	poke(t, s)
	first := waitForTaskCount(t, s, 1)[0]

	// The next run is due while the first still runs: it waits.
	testClock.Advance(5 * time.Minute)
	poke(t, s)
	waitForSchedule(t, s, scheduleReason("WaitingForActiveRun"))
	if n := len(scheduleTasks(t, s)); n != 1 {
		t.Fatalf("%d tasks, want 1 while the first runs", n)
	}

	// Once the first finishes, the waiting run starts (still within the deadline).
	finishJob(t, "forbid", first.Name, true, time.Now())
	tasks := waitForTaskCount(t, s, 2)
	if tasks[0].Name != first.Name {
		t.Errorf("tasks = %s, %s", tasks[0].Name, tasks[1].Name)
	}
	waitForSchedule(t, s, func(s *kwerftv1.Schedule) error {
		if s.Status.LastSuccessTime == nil || !slices.Equal(s.Status.Active, []string{tasks[1].Name}) {
			return fmt.Errorf("status = %+v", s.Status)
		}
		return nil
	})
}

func TestScheduleReplaceStopsActiveRun(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "replace")
	testClock.Reset()
	spec := everyFiveMinutes()
	spec.Concurrency = kwerftv1.ConcurrencyReplace
	s := createSchedule(t, "replace", "poll", spec)
	testClock.Advance(6 * time.Minute)
	poke(t, s)
	first := waitForTaskCount(t, s, 1)[0]

	testClock.Advance(5 * time.Minute)
	poke(t, s)
	eventually(t, func() error {
		tasks := scheduleTasks(t, s)
		if len(tasks) != 1 || tasks[0].Name == first.Name {
			return fmt.Errorf("tasks = %v, want only a newer run than %s", tasks, first.Name)
		}
		return nil
	})
}

func TestSchedulePrunesHistory(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "history")
	testClock.Reset()
	spec := everyFiveMinutes()
	spec.Concurrency = kwerftv1.ConcurrencyAllow
	spec.History = kwerftv1.ScheduleHistory{Succeeded: ptr.To[int32](1), Failed: ptr.To[int32](1)}
	s := createSchedule(t, "history", "backup", spec)

	var tasks []kwerftv1.Task
	for i := 1; i <= 4; i++ {
		testClock.Advance(5 * time.Minute)
		poke(t, s)
		tasks = waitForTaskCount(t, s, i)
	}
	// Two successes, two failures, finished in this order.
	base := time.Now().Add(-10 * time.Minute)
	finishJob(t, "history", tasks[0].Name, true, base)
	finishJob(t, "history", tasks[1].Name, false, base.Add(time.Minute))
	finishJob(t, "history", tasks[2].Name, true, base.Add(2*time.Minute))
	finishJob(t, "history", tasks[3].Name, false, base.Add(3*time.Minute))

	var left []kwerftv1.Task
	eventually(t, func() error {
		left = scheduleTasks(t, s)
		var names []string
		for _, t := range left {
			names = append(names, t.Name)
		}
		if !slices.Equal(names, []string{tasks[2].Name, tasks[3].Name}) {
			return fmt.Errorf("left %v, want the newest success and failure", names)
		}
		return nil
	})
	waitForSchedule(t, s, func(s *kwerftv1.Schedule) error {
		if s.Status.LastSuccessTime == nil || s.Status.LastFailureTime == nil || len(s.Status.Active) != 0 {
			return fmt.Errorf("status = %+v", s.Status)
		}
		if !s.Status.LastFailureTime.After(s.Status.LastSuccessTime.Time) {
			return fmt.Errorf("lastFailureTime %v should be after lastSuccessTime %v", s.Status.LastFailureTime, s.Status.LastSuccessTime)
		}
		return nil
	})
}

func TestScheduleInvalidCron(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "badcron")
	spec := everyFiveMinutes()
	spec.Schedule = "every tuesday"
	s := createSchedule(t, "badcron", "oops", spec)
	waitForSchedule(t, s, scheduleReason("InvalidSchedule"))
}
