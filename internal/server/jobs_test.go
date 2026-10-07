// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

func imageTaskBody(image string) map[string]any {
	return map[string]any{"spec": map[string]any{"source": map[string]any{"image": map[string]any{"ref": image}}}}
}

func scheduleBody(name, cron string) map[string]any {
	return map[string]any{"name": name, "spec": map[string]any{
		"schedule": cron,
		"task": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "ghcr.io/acme/ops:1.4.0"}},
			"env":    []map[string]string{{"name": "TARGET", "value": "s3"}},
		},
	}}
}

// listTasks polls the task list until check passes.
func listTasks(t *testing.T, s *session, query string, check func([]taskJSON) error) []taskJSON {
	t.Helper()
	var list []taskJSON
	eventually(t, func() error {
		list = nil
		if code := s.do(t, "GET", "/api/v1/tasks"+query, nil, &list); code != http.StatusOK {
			return fmt.Errorf("list tasks: %d", code)
		}
		return check(list)
	})
	return list
}

func names[T any](list []T, name func(T) string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, name(x))
	}
	return out
}

func taskNames(list []taskJSON) []string {
	return names(list, func(t taskJSON) string { return t.Name })
}

func TestJobRolesAreEnforcedByKubernetesRBAC(t *testing.T) {
	c := newConsole(t)
	c.project(t, "jobs-rbac")
	const base = "/api/v1/projects/jobs-rbac"
	for _, tc := range []struct {
		what, path string
		body       any
	}{
		{"task", base + "/tasks", imageTaskBody("busybox:1.37")},
		{"schedule", base + "/schedules", scheduleBody("backup", "15 1 * * *")},
		{"volume", base + "/volumes", map[string]string{"name": "data", "size": "1Gi", "class": "hcloud-volume"}},
	} {
		var e apiError
		if code := c.viewer.do(t, "POST", tc.path, tc.body, &e); code != http.StatusForbidden || e.Error != "Your role does not allow this." {
			t.Errorf("viewer creates a %s: %d %+v, want 403", tc.what, code, e)
		}
		if code := c.dev.do(t, "POST", tc.path, tc.body, nil); code != http.StatusCreated {
			t.Errorf("developer creates a %s: %d, want 201", tc.what, code)
		}
	}
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/schedules/backup/suspend", nil},
		{"POST", base + "/schedules/backup/run", map[string]any{"envOverrides": []map[string]string{{"name": "FULL", "value": "1"}}}},
		{"DELETE", base + "/schedules/backup", nil},
		{"PATCH", base + "/volumes/data", map[string]string{"size": "2Gi"}},
		{"DELETE", base + "/volumes/data", nil},
	} {
		if code := c.viewer.do(t, req.method, req.path, req.body, nil); code != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d, want 403", req.method, req.path, code)
		}
	}
	// Viewers read everything, including a single Task.
	tasks := listTasks(t, c.viewer, "?project=jobs-rbac", func(l []taskJSON) error {
		if len(l) != 1 {
			return fmt.Errorf("tasks = %v", taskNames(l))
		}
		return nil
	})
	if code := c.viewer.do(t, "GET", base+"/tasks/"+tasks[0].Name, nil, nil); code != http.StatusOK {
		t.Errorf("viewer gets a task: %d", code)
	}
	if code := c.viewer.do(t, "POST", base+"/tasks/"+tasks[0].Name+"/cancel", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer cancels a task: %d, want 403", code)
	}
	if code := c.viewer.do(t, "GET", base+"/schedules/backup", nil, nil); code != http.StatusOK {
		t.Errorf("viewer gets a schedule: %d", code)
	}

	entries, err := c.store.RecentAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := names(entries, func(e store.AuditEntry) string { return e.Actor + " " + e.Action + " " + e.Target })
	for _, want := range []string{
		"viewer@example.com schedule.create.denied jobs-rbac/backup",
		"viewer@example.com volume.create.denied jobs-rbac/data",
		"developer@example.com schedule.create jobs-rbac/backup",
		"developer@example.com volume.create jobs-rbac/data",
		"developer@example.com task.create jobs-rbac/" + tasks[0].Name,
	} {
		if !slices.Contains(seen, want) {
			t.Errorf("audit lacks %q: %v", want, seen)
		}
	}
}

func TestRunNowFromAnApp(t *testing.T) {
	c := newConsole(t)
	c.project(t, "runs")
	c.project(t, "runs-other")
	ctx := context.Background()
	app := imageApp("api", "ghcr.io/acme/api:1.0")
	app["spec"].(map[string]any)["env"] = []map[string]string{{"name": "MODE", "value": "server"}}
	if code := c.dev.do(t, "POST", "/api/v1/projects/runs/apps", app, nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/runs-other/tasks", imageTaskBody("busybox:1.37"), nil); code != http.StatusCreated {
		t.Fatalf("create task elsewhere: %d", code)
	}

	run := map[string]any{"spec": map[string]any{
		"fromApp":      "api",
		"envOverrides": []map[string]string{{"name": "FORCE", "value": "1"}},
		"timeout":      "30m",
		"onSuccess":    map[string]any{"restart": []string{"api"}},
	}}
	var task kwerftv1.Task
	if code := c.dev.do(t, "POST", "/api/v1/projects/runs/tasks", run, &task); code != http.StatusCreated {
		t.Fatalf("run now: %d", code)
	}
	if !strings.HasPrefix(task.Name, "api-run-") || task.Kind != "Task" || task.ManagedFields != nil {
		t.Errorf("task = %s %s", task.Kind, task.Name)
	}
	if task.Spec.FromApp != "api" || len(task.Spec.EnvOverrides) != 1 || task.Spec.EnvOverrides[0].Value != "1" {
		t.Errorf("spec = %+v", task.Spec)
	}
	if by := task.Annotations[kwerftv1.AnnotationStartedBy]; by != "developer@example.com" {
		t.Errorf("started by %q", by)
	}

	// The reconciler runs it with the App's env and the override.
	eventually(t, func() error {
		var job batchv1.Job
		if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "runs", Name: task.Name}, &job); err != nil {
			return err
		}
		env := job.Spec.Template.Spec.Containers[0].Env
		if !slices.ContainsFunc(env, func(e corev1.EnvVar) bool { return e.Name == "MODE" && e.Value == "server" }) ||
			!slices.ContainsFunc(env, func(e corev1.EnvVar) bool { return e.Name == "FORCE" && e.Value == "1" }) {
			return fmt.Errorf("job env = %v", env)
		}
		return nil
	})

	// Lists filter by project, app, phase and schedule.
	list := listTasks(t, c.viewer, "?project=runs&app=api", func(l []taskJSON) error {
		if len(l) != 1 || l[0].Name != task.Name {
			return fmt.Errorf("tasks = %v", taskNames(l))
		}
		return nil
	})
	got := list[0]
	if got.StartedBy != "developer@example.com" || got.FromApp != "api" || !slices.Equal(got.Overrides, []string{"FORCE"}) ||
		!slices.Equal(got.Restart, []string{"api"}) || got.Schedule != "" || got.Phase != "pending" {
		t.Errorf("summary = %+v", got)
	}
	listTasks(t, c.viewer, "?project=runs-other", func(l []taskJSON) error {
		if len(l) != 1 || l[0].Project != "runs-other" {
			return fmt.Errorf("tasks = %v", taskNames(l))
		}
		return nil
	})
	listTasks(t, c.viewer, "?project=runs&app=other", func(l []taskJSON) error {
		if len(l) != 0 {
			return fmt.Errorf("tasks = %v", taskNames(l))
		}
		return nil
	})
	listTasks(t, c.viewer, "?project=runs&phase=failed", func(l []taskJSON) error {
		if len(l) != 0 {
			return fmt.Errorf("tasks = %v", taskNames(l))
		}
		return nil
	})

	// Cancel stops it and keeps the record; it is final.
	path := "/api/v1/projects/runs/tasks/" + task.Name
	if code := c.dev.do(t, "POST", path+"/cancel", nil, nil); code != http.StatusOK {
		t.Fatalf("cancel: %d", code)
	}
	eventually(t, func() error {
		var got kwerftv1.Task
		if code := c.viewer.do(t, "GET", path, nil, &got); code != http.StatusOK {
			return fmt.Errorf("get: %d", code)
		}
		ready := meta.FindStatusCondition(got.Status.Conditions, controllers.ConditionReady)
		if got.Status.Phase != kwerftv1.TaskFailed || ready == nil || ready.Reason != "Cancelled" {
			return fmt.Errorf("phase %s, ready %+v", got.Status.Phase, ready)
		}
		return nil
	})
	listTasks(t, c.viewer, "?project=runs&phase=Failed", func(l []taskJSON) error {
		if len(l) != 1 || l[0].Reason != "Cancelled" || l[0].Finished == nil {
			return fmt.Errorf("tasks = %+v", l)
		}
		return nil
	})
	var e apiError
	if code := c.dev.do(t, "POST", path+"/cancel", nil, &e); code != http.StatusConflict {
		t.Errorf("cancel a finished run: %d %+v, want 409", code, e)
	}
	if code := c.dev.do(t, "DELETE", path, nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if code := c.dev.do(t, "GET", path, nil, &e); code != http.StatusNotFound || !strings.Contains(e.Error, task.Name) {
		t.Errorf("get after delete: %d %+v, want 404", code, e)
	}
}

func TestTaskValidationNamesTheField(t *testing.T) {
	c := newConsole(t)
	c.project(t, "task-checks")
	for _, tc := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"missing app", map[string]any{"spec": map[string]any{"fromApp": "nope"}}, "spec.fromApp"},
		{"bad override", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "envOverrides": []map[string]string{{"name": "1BAD", "value": "x"}}}}, "spec.envOverrides[0].name"},
		{"missing volume", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "volumes": []map[string]any{{"path": "/data", "volume": "nope"}}}}, "spec.volumes[0].volume"},
		{"bad secret name", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "volumes": []map[string]any{{"path": "/keys", "secret": "SSH_Key"}}}}, "spec.volumes[0].secret"},
		{"path mounted twice", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "volumes": []map[string]any{{"path": "/keys", "secret": "a"}, {"path": "/keys", "secret": "b"}}}}, "spec.volumes[1].path"},
		{"missing restart app", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "onSuccess": map[string]any{"restart": []string{"nope"}}}}, "spec.onSuccess.restart[0]"},
		{"too many retries", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "retries": 11}}, "spec.retries"},
		{"no time to stop", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "stopSeconds": 0}}, "spec.stopSeconds"},
		{"too long to stop", map[string]any{"spec": map[string]any{
			"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "stopSeconds": 3601}}, "spec.stopSeconds"},
		{"bad name", map[string]any{"name": "Bad_Name", "spec": map[string]any{"source": map[string]any{"image": map[string]any{"ref": "busybox"}}}}, "name"},
	} {
		var e apiError
		code := c.dev.do(t, "POST", "/api/v1/projects/task-checks/tasks", tc.body, &e)
		t.Logf("%s: %s", tc.name, e.Error)
		if code != http.StatusUnprocessableEntity || e.Field != tc.field || e.Error == "" {
			t.Errorf("%s: %d %+v, want 422 on %s", tc.name, code, e, tc.field)
		}
	}
	var e apiError
	if code := c.dev.do(t, "POST", "/api/v1/projects/task-checks/tasks", map[string]any{"spec": map[string]any{}}, &e); code != http.StatusUnprocessableEntity {
		t.Errorf("neither source nor fromApp: %d %+v, want 422", code, e)
	}
	// A Secret need not exist yet: the pod waits for it.
	withSecret := map[string]any{"spec": map[string]any{
		"source": map[string]any{"image": map[string]any{"ref": "busybox"}}, "volumes": []map[string]any{{"path": "/keys", "secret": "ssh-key", "mode": 0o400}}}}
	var task kwerftv1.Task
	if code := c.dev.do(t, "POST", "/api/v1/projects/task-checks/tasks", withSecret, &task); code != http.StatusCreated {
		t.Fatalf("task with a secret: %d", code)
	}
	if v := task.Spec.Volumes; len(v) != 1 || v[0].Secret != "ssh-key" || v[0].Mode == nil || *v[0].Mode != 0o400 {
		t.Errorf("volumes = %+v", v)
	}
}

func TestRunNowFromASchedule(t *testing.T) {
	c := newConsole(t)
	c.project(t, "nightly")
	ctx := context.Background()
	const base = "/api/v1/projects/nightly/schedules"
	var created kwerftv1.Schedule
	if code := c.dev.do(t, "POST", base, scheduleBody("backup", "15 1 * * *"), &created); code != http.StatusCreated {
		t.Fatalf("create schedule: %d", code)
	}
	if code := c.dev.do(t, "POST", base, scheduleBody("other", "0 4 * * *"), nil); code != http.StatusCreated {
		t.Fatalf("create second schedule: %d", code)
	}

	var task kwerftv1.Task
	run := map[string]any{"envOverrides": []map[string]string{{"name": "FULL", "value": "1"}, {"name": "TARGET", "value": "local"}}}
	if code := c.dev.do(t, "POST", base+"/backup/run", run, &task); code != http.StatusCreated {
		t.Fatalf("run now: %d", code)
	}
	if !strings.HasPrefix(task.Name, "backup-manual-") {
		t.Errorf("name = %s", task.Name)
	}
	if task.Labels[controllers.LabelSchedule] != "backup" {
		t.Errorf("labels = %v", task.Labels)
	}
	owner := metav1.GetControllerOf(&task)
	if owner == nil || owner.Kind != "Schedule" || owner.Name != "backup" || owner.UID != created.UID {
		t.Errorf("controller = %+v, want Schedule backup (%s)", owner, created.UID)
	}
	if task.Spec.Source == nil || task.Spec.Source.Image.Ref != "ghcr.io/acme/ops:1.4.0" || len(task.Spec.Env) != 1 {
		t.Errorf("spec not copied from the template: %+v", task.Spec)
	}
	if !slices.Equal(names(task.Spec.EnvOverrides, func(e corev1.EnvVar) string { return e.Name + "=" + e.Value }), []string{"FULL=1", "TARGET=local"}) {
		t.Errorf("overrides = %v", task.Spec.EnvOverrides)
	}
	// Without a body it runs the template as it is.
	if code := c.dev.do(t, "POST", base+"/other/run", nil, nil); code != http.StatusCreated {
		t.Errorf("run without overrides: %d", code)
	}

	// The Schedule counts it as its own run.
	eventually(t, func() error {
		var s kwerftv1.Schedule
		if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "nightly", Name: "backup"}, &s); err != nil {
			return err
		}
		if !slices.Contains(s.Status.Active, task.Name) {
			return fmt.Errorf("active = %v", s.Status.Active)
		}
		return nil
	})
	listTasks(t, c.viewer, "?project=nightly&schedule=backup", func(l []taskJSON) error {
		if len(l) != 1 || l[0].Name != task.Name || l[0].Schedule != "backup" || l[0].StartedBy != "developer@example.com" {
			return fmt.Errorf("tasks = %+v", l)
		}
		return nil
	})
	var schedules []scheduleJSON
	eventually(t, func() error {
		c.viewer.do(t, "GET", "/api/v1/schedules?project=nightly", nil, &schedules)
		if len(schedules) != 2 || schedules[0].Name != "backup" {
			return fmt.Errorf("schedules = %+v", schedules)
		}
		s := schedules[0]
		if s.LastRun == nil || s.LastRun.Name != task.Name || s.Phase != "scheduled" || s.NextRun == nil || s.Image != "ghcr.io/acme/ops:1.4.0" || s.Concurrency != "Forbid" {
			return fmt.Errorf("summary = %+v", s)
		}
		return nil
	})
}

func TestScheduleSuspendResumeAndUpdate(t *testing.T) {
	c := newConsole(t)
	c.project(t, "cron")
	const path = "/api/v1/projects/cron/schedules/report"
	if code := c.dev.do(t, "POST", "/api/v1/projects/cron/schedules", scheduleBody("report", "@daily"), nil); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	summary := func(check func(scheduleJSON) error) {
		t.Helper()
		eventually(t, func() error {
			var list []scheduleJSON
			c.viewer.do(t, "GET", "/api/v1/schedules?project=cron", nil, &list)
			if len(list) != 1 {
				return fmt.Errorf("schedules = %+v", list)
			}
			return check(list[0])
		})
	}
	summary(func(s scheduleJSON) error {
		if s.Phase != "scheduled" || s.NextRun == nil {
			return fmt.Errorf("summary = %+v", s)
		}
		return nil
	})

	var s kwerftv1.Schedule
	if code := c.dev.do(t, "POST", path+"/suspend", nil, &s); code != http.StatusOK || !s.Spec.Suspend {
		t.Fatalf("suspend: %d %+v", code, s.Spec)
	}
	summary(func(s scheduleJSON) error {
		if s.Phase != "suspended" || !s.Suspend || s.NextRun != nil {
			return fmt.Errorf("summary = %+v", s)
		}
		return nil
	})
	s = kwerftv1.Schedule{}
	if code := c.dev.do(t, "POST", path+"/resume", nil, &s); code != http.StatusOK || s.Spec.Suspend {
		t.Fatalf("resume: %d %+v", code, s.Spec)
	}
	summary(func(s scheduleJSON) error {
		if s.Phase != "scheduled" || s.Suspend || s.NextRun == nil {
			return fmt.Errorf("summary = %+v", s)
		}
		return nil
	})

	// Update: guarded by the generation, validated like create.
	if code := c.dev.do(t, "GET", path, nil, &s); code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	spec := s.Spec.DeepCopy()
	spec.Schedule, spec.TimeZone = "30 3 * * 1-5", "Europe/Berlin"
	var updated kwerftv1.Schedule
	if code := c.dev.do(t, "PUT", path, map[string]any{"spec": spec, "generation": s.Generation}, &updated); code != http.StatusOK || updated.Spec.Schedule != "30 3 * * 1-5" {
		t.Fatalf("update: %d %+v", code, updated.Spec)
	}
	var e apiError
	if code := c.dev.do(t, "PUT", path, map[string]any{"spec": spec, "generation": s.Generation}, &e); code != http.StatusConflict {
		t.Errorf("update of a stale generation: %d %+v, want 409", code, e)
	}
	for _, tc := range []struct {
		schedule, tz, field string
	}{
		{"every day", "", "spec.schedule"},
		{"61 * * * *", "", "spec.schedule"},
		{"0 3 * * *", "Mars/Olympus", "spec.timeZone"},
	} {
		bad := spec.DeepCopy()
		bad.Schedule, bad.TimeZone = tc.schedule, tc.tz
		e = apiError{}
		code := c.dev.do(t, "PUT", path, map[string]any{"spec": bad, "generation": updated.Generation}, &e)
		t.Logf("%q %q: %s", tc.schedule, tc.tz, e.Error)
		if code != http.StatusUnprocessableEntity || e.Field != tc.field {
			t.Errorf("schedule %q tz %q: %d %+v, want 422 on %s", tc.schedule, tc.tz, code, e, tc.field)
		}
	}
	e = apiError{}
	long := scheduleBody(strings.Repeat("a", 53), "@daily")
	if code := c.dev.do(t, "POST", "/api/v1/projects/cron/schedules", long, &e); code != http.StatusUnprocessableEntity || e.Field != "name" {
		t.Errorf("53-character name: %d %+v, want 422 on name", code, e)
	}
	e = apiError{}
	badTask := scheduleBody("bad-task", "@daily")
	badTask["spec"].(map[string]any)["task"] = map[string]any{"fromApp": "nope"}
	if code := c.dev.do(t, "POST", "/api/v1/projects/cron/schedules", badTask, &e); code != http.StatusUnprocessableEntity || e.Field != "spec.task.fromApp" {
		t.Errorf("schedule from a missing app: %d %+v, want 422 on spec.task.fromApp", code, e)
	}

	// The form's preview uses the reconciler's parser.
	var preview struct {
		Next     []string `json:"next"`
		TimeZone string   `json:"timeZone"`
	}
	q := url.Values{"schedule": {"30 3 * * *"}, "timeZone": {"Europe/Berlin"}}
	if code := c.viewer.do(t, "GET", "/api/v1/schedules/preview?"+q.Encode(), nil, &preview); code != http.StatusOK || len(preview.Next) != 3 || preview.TimeZone != "Europe/Berlin" {
		t.Fatalf("preview: %d %+v", code, preview)
	}
	for _, n := range preview.Next {
		at, err := time.Parse(time.RFC3339, n)
		berlin, _ := time.LoadLocation("Europe/Berlin")
		if err != nil || at.In(berlin).Hour() != 3 || at.In(berlin).Minute() != 30 {
			t.Errorf("next run %q is not 03:30 in Berlin", n)
		}
	}
	e = apiError{}
	if code := c.viewer.do(t, "GET", "/api/v1/schedules/preview?schedule=nope", nil, &e); code != http.StatusUnprocessableEntity || e.Field != "schedule" {
		t.Errorf("preview of a bad schedule: %d %+v", code, e)
	}

	if code := c.dev.do(t, "DELETE", path, nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
}

func TestVolumesResizeAndDeleteWhileMounted(t *testing.T) {
	c := newConsole(t)
	c.project(t, "disks")
	c.project(t, "disks-other")
	const base = "/api/v1/projects/disks/volumes"
	var v volumeJSON
	if code := c.dev.do(t, "POST", base, map[string]string{"name": "data", "size": "1Gi", "class": "hcloud-volume"}, &v); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if v.Size != "1Gi" || v.Class != "hcloud-volume" || v.Project != "disks" {
		t.Errorf("created = %+v", v)
	}
	if code := c.dev.do(t, "POST", base, map[string]string{"name": "scratch", "size": "5"}, &v); code != http.StatusCreated || v.Size != "5Gi" || v.Class != "local-nvme" {
		t.Errorf("create with a bare number: %d %+v", code, v)
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/disks-other/volumes", map[string]string{"name": "data", "size": "1Gi"}, nil); code != http.StatusCreated {
		t.Errorf("create in another project: %d", code)
	}

	var e apiError
	for _, tc := range []struct {
		method, path string
		body         map[string]string
		field        string
	}{
		{"POST", base, map[string]string{"name": "x", "size": "lots"}, "size"},
		{"POST", base, map[string]string{"name": "x", "size": "-1Gi"}, "size"},
		{"POST", base, map[string]string{"name": "Not_Valid", "size": "1Gi"}, "name"},
		{"POST", base, map[string]string{"name": "x", "size": "1Gi", "class": "ssd"}, "class"},
		{"PATCH", base + "/data", map[string]string{"size": "500Mi"}, "size"},   // shrink
		{"PATCH", base + "/scratch", map[string]string{"size": "10Gi"}, "size"}, // local-nvme
	} {
		e = apiError{}
		code := c.dev.do(t, tc.method, tc.path, tc.body, &e)
		t.Logf("%s %s %v: %s", tc.method, tc.path, tc.body, e.Error)
		if code != http.StatusUnprocessableEntity || e.Field != tc.field {
			t.Errorf("%s %s %v: %d %+v, want 422 on %s", tc.method, tc.path, tc.body, code, e, tc.field)
		}
	}
	v = volumeJSON{}
	if code := c.dev.do(t, "PATCH", base+"/data", map[string]string{"size": "2Gi"}, &v); code != http.StatusOK || v.Size != "2Gi" || v.Name != "data" {
		t.Errorf("grow: %d %+v", code, v)
	}

	// An App mounts it; deleting the Volume waits and says why.
	app := imageApp("web", "nginx:1.27")
	app["spec"].(map[string]any)["volumes"] = []map[string]any{{"path": "/data", "volume": "data", "readOnly": true}}
	if code := c.dev.do(t, "POST", "/api/v1/projects/disks/apps", app, nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	var waiting struct {
		UsedBy  []string `json:"usedBy"`
		Reason  string   `json:"reason"`
		Message string   `json:"message"`
	}
	if code := c.dev.do(t, "DELETE", base+"/data", nil, &waiting); code != http.StatusAccepted || !slices.Equal(waiting.UsedBy, []string{"App/web"}) || waiting.Reason != "InUse" {
		t.Fatalf("delete while mounted: %d %+v, want 202 naming App/web", code, waiting)
	}
	volumes := func(check func([]volumeJSON) error) {
		t.Helper()
		eventually(t, func() error {
			var list []volumeJSON
			if code := c.viewer.do(t, "GET", "/api/v1/volumes?project=disks", nil, &list); code != http.StatusOK {
				return fmt.Errorf("list: %d", code)
			}
			return check(list)
		})
	}
	volumes(func(l []volumeJSON) error {
		i := slices.IndexFunc(l, func(v volumeJSON) bool { return v.Name == "data" })
		if i < 0 || l[i].Phase != "deleting" || l[i].Reason != "InUse" || !strings.Contains(l[i].Message, "App/web") || !slices.Equal(l[i].UsedBy, []string{"App/web"}) {
			return fmt.Errorf("volumes = %+v", l)
		}
		if len(l) != 2 {
			return fmt.Errorf("the list is not filtered by project: %+v", l)
		}
		return nil
	})
	// Once the App is gone, so is the Volume.
	if code := c.dev.do(t, "DELETE", "/api/v1/projects/disks/apps/web", nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete app: %d", code)
	}
	volumes(func(l []volumeJSON) error {
		if slices.ContainsFunc(l, func(v volumeJSON) bool { return v.Name == "data" }) {
			return fmt.Errorf("volumes = %+v", l)
		}
		return nil
	})
	if code := c.dev.do(t, "DELETE", base+"/scratch", nil, nil); code != http.StatusNoContent {
		t.Errorf("delete an unused volume: %d, want 204", code)
	}
	var all []volumeJSON
	c.viewer.do(t, "GET", "/api/v1/volumes", nil, &all)
	if !slices.ContainsFunc(all, func(v volumeJSON) bool { return v.Project == "disks-other" }) {
		t.Errorf("all volumes = %+v", all)
	}
}

func TestDomainsListAcrossProjects(t *testing.T) {
	c := newConsole(t)
	c.project(t, "dom-a")
	c.project(t, "dom-b")
	ctx := context.Background()
	app := imageApp("shop", "nginx:1.27")
	app["spec"].(map[string]any)["ports"] = []map[string]any{{"container": 8080, "public": "shop.example.com"}}
	if code := c.dev.do(t, "POST", "/api/v1/projects/dom-a/apps", app, nil); code != http.StatusCreated {
		t.Fatalf("create app: %d", code)
	}
	// A hand-made Domain in another project, claiming the same hostname.
	if err := cluster.admin.Create(ctx, &kwerftv1.Domain{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dom-b", Name: "shop-copy"},
		Spec:       kwerftv1.DomainSpec{Hostname: "shop.example.com"},
	}); err != nil {
		t.Fatal(err)
	}

	// Stand in for the Domain reconciler (not running here).
	notAfter := metav1.NewTime(time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC))
	setStatus := func(ns, name string, status metav1.ConditionStatus, reason string, listener string, expires *metav1.Time) {
		t.Helper()
		eventually(t, func() error {
			var d kwerftv1.Domain
			if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
				return err
			}
			d.Status.Listener, d.Status.NotAfter = listener, expires
			meta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
				Type: controllers.ConditionReady, Status: status, Reason: reason, Message: reason, ObservedGeneration: d.Generation,
			})
			return cluster.admin.Status().Update(ctx, &d)
		})
	}
	setStatus("dom-a", "shop.example.com", metav1.ConditionTrue, "CertificateIssued", "https-shop-example-com", &notAfter)
	setStatus("dom-b", "shop-copy", metav1.ConditionFalse, "HostnameConflict", "", nil)

	var list []domainJSON
	eventually(t, func() error {
		list = nil
		if code := c.viewer.do(t, "GET", "/api/v1/domains", nil, &list); code != http.StatusOK {
			return fmt.Errorf("list: %d", code)
		}
		mine := slices.DeleteFunc(slices.Clone(list), func(d domainJSON) bool { return d.Project != "dom-a" && d.Project != "dom-b" })
		if len(mine) != 2 || mine[0].Certificate != "valid" || mine[1].Certificate != "failed" {
			return fmt.Errorf("domains = %+v", mine)
		}
		return nil
	})
	i := slices.IndexFunc(list, func(d domainJSON) bool { return d.Project == "dom-a" })
	if d := list[i]; d.Hostname != "shop.example.com" || d.App != "shop" || d.Listener != "https-shop-example-com" ||
		d.NotAfter == nil || !d.NotAfter.Equal(notAfter.Time) || d.Reason != "CertificateIssued" {
		t.Errorf("dom-a domain = %+v", d)
	}
	j := slices.IndexFunc(list, func(d domainJSON) bool { return d.Project == "dom-b" })
	if d := list[j]; d.App != "" || d.Reason != "HostnameConflict" || d.NotAfter != nil {
		t.Errorf("dom-b domain = %+v", d)
	}
	var one []domainJSON
	c.viewer.do(t, "GET", "/api/v1/domains?project=dom-b", nil, &one)
	if len(one) != 1 || one[0].Name != "shop-copy" {
		t.Errorf("filtered = %+v", one)
	}
}

// A run the kernel stopped for its memory limit says so, with the limit, in
// the summary the Jobs view and the alerts read; exit code 137 alone does not.
func TestTaskSummaryOutOfMemory(t *testing.T) {
	limit := resource.MustParse("256Mi")
	task := &kwerftv1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "sync-1", Namespace: "shop"},
		Status:     kwerftv1.TaskStatus{Phase: kwerftv1.TaskFailed, ExitCode: ptr.To[int32](137), TerminationReason: "OOMKilled", MemoryLimit: &limit},
	}
	s := taskSummary(task)
	if s.ExitCode == nil || *s.ExitCode != 137 || s.TerminationReason != "OOMKilled" || s.MemoryLimit != "256Mi" {
		t.Errorf("summary = exit %v, reason %q, limit %q", s.ExitCode, s.TerminationReason, s.MemoryLimit)
	}
	task.Status.TerminationReason, task.Status.MemoryLimit = "Error", nil
	if s := taskSummary(task); s.TerminationReason != "Error" || s.MemoryLimit != "" {
		t.Errorf("summary = reason %q, limit %q; want Error and none", s.TerminationReason, s.MemoryLimit)
	}
}
