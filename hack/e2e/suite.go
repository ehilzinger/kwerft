package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// ---- the suite -------------------------------------------------------------------

const project = "e2e"

// suite creates what it checks up front, then waits for the checks side by
// side, so each one's time is measured from what it waits for: an App on
// HTTPS from its creation, a build from "Build now", an alert from the
// crashing App's creation.
func (r *runner) suite(ctx context.Context) error {
	web, gitHost := "web."+r.domain(), "git."+r.domain()
	marker := "kwerft-e2e-marker-" + r.cfg.RunID

	var createdAt, crashAt, buildAt time.Time
	var buildName string
	if err := r.step("Project and apps created", func() (string, error) {
		if err := r.createProject(ctx, project); err != nil {
			return "", fmt.Errorf("project: %w", err)
		}
		if err := r.createApp(ctx, project, "web", whoamiSpec(web)); err != nil {
			return "", fmt.Errorf("app web: %w", err)
		}
		createdAt = r.now()
		crash := kwerftv1.AppSpec{
			Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: busyboxImage}},
			Command: []string{"sh", "-c", "echo crashing on purpose; exit 1"},
		}
		if err := r.createApp(ctx, project, "crash", crash); err != nil {
			return "", fmt.Errorf("app crash: %w", err)
		}
		crashAt = r.now()
		gitSpec := kwerftv1.AppSpec{
			Source: kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: r.cfg.GitRepo, Branch: r.cfg.GitBranch, Builder: "dockerfile"}},
			Ports:  []kwerftv1.AppPort{{Container: 80, Public: gitHost}},
		}
		if err := r.createApp(ctx, project, "git", gitSpec); err != nil {
			return "", fmt.Errorf("app git: %w", err)
		}
		b, err := r.console.buildNow(ctx, project, "git")
		if err != nil {
			return "", fmt.Errorf("build now: %w", err)
		}
		buildAt, buildName = r.now(), b.Name
		return "web, crash, git (build " + b.Name + ")", nil
	}); err != nil {
		return err
	}

	// Each lane runs its checks in order; lanes run side by side. Results are
	// reported in this order whatever finishes first.
	lanes := [][]check{
		{
			{"App on HTTPS", createdAt, 0, func() (string, error) { return r.checkWeb(ctx, web) }},
			{"Task runs and restarts web", time.Time{}, 0, func() (string, error) { return r.checkTask(ctx, web, marker) }},
			{"Log search", time.Time{}, 0, func() (string, error) { return r.checkLogs(ctx, marker) }},
		},
		{{"Git build deploys", buildAt, 3 * time.Minute, func() (string, error) { return r.checkBuild(ctx, buildName, buildAt, gitHost) }}},
		{{"Metrics", time.Time{}, 0, func() (string, error) { return r.checkMetrics(ctx) }}},
		{{"Crash loop alert", crashAt, 2 * time.Minute, func() (string, error) { return r.checkAlert(ctx, crashAt) }}},
	}
	results := make([][]result, len(lanes))
	var wg sync.WaitGroup
	for i, lane := range lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, c := range lane {
				if ctx.Err() != nil {
					results[i] = append(results[i], result{Name: c.name, Status: skip, Detail: "the run timed out or was cancelled"})
					continue
				}
				results[i] = append(results[i], r.measure(c.name, c.since, c.budget, c.fn))
			}
		}()
	}
	wg.Wait()
	failed := false
	for _, lane := range results {
		for _, res := range lane {
			r.rep.add(res)
			failed = failed || res.Status == fail || res.Status == skip
		}
	}
	if failed {
		return errors.New("checks failed")
	}
	return nil
}

type check struct {
	name   string
	since  time.Time
	budget time.Duration
	fn     func() (string, error)
}

func (r *runner) checkWeb(ctx context.Context, web string) (string, error) {
	if err := r.waitHTTPS(ctx, "https://"+web+"/", "Hostname", 10*time.Minute); err != nil {
		return "", err
	}
	detail := fmt.Sprintf("https://%s, certificate from %s", web, r.certs.issuerOf(web))
	if r.cfg.mode() == modeFresh {
		took := r.now().Sub(r.sshAt)
		detail += fmt.Sprintf("; fresh server to app on HTTPS in %s", fmtDuration(took))
		if took > 10*time.Minute {
			r.rep.note("Fresh server to app on HTTPS took %s, over the 10 min of the Phase 1 exit criterion.", fmtDuration(took))
		}
	}
	return detail, nil
}

func (r *runner) checkTask(ctx context.Context, web, marker string) (string, error) {
	before, err := r.console.appPods(ctx, project, "web")
	if err != nil {
		return "", err
	}
	name, err := r.console.createTask(ctx, project, kwerftv1.TaskSpec{
		Source:    &kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: busyboxImage}},
		Command:   []string{"sh", "-c", "echo " + marker},
		Env:       []corev1.EnvVar{{Name: "E2E_RUN", Value: r.cfg.RunID}},
		Timeout:   &metav1.Duration{Duration: 5 * time.Minute},
		OnSuccess: &kwerftv1.TaskOnSuccess{Restart: []string{"web"}},
	})
	if err != nil {
		return "", err
	}
	err = r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		t, err := r.console.task(ctx, project, name)
		if err != nil {
			return false, err
		}
		switch t.Status.Phase {
		case kwerftv1.TaskSucceeded:
			return true, nil
		case kwerftv1.TaskFailed:
			code := "unknown"
			if t.Status.ExitCode != nil {
				code = strconv.Itoa(int(*t.Status.ExitCode))
			}
			return true, fmt.Errorf("task %s failed (exit %s)", name, code)
		}
		return false, fmt.Errorf("task %s is %s", name, cmpOr(string(t.Status.Phase), "pending"))
	})
	if err != nil {
		return "", err
	}
	old := map[string]bool{}
	for _, p := range before {
		old[p.Name] = true
	}
	err = r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		pods, err := r.console.appPods(ctx, project, "web")
		if err != nil {
			return false, err
		}
		for _, p := range pods {
			if !old[p.Name] && p.Ready {
				return true, nil
			}
		}
		return false, errors.New("web has not been restarted")
	})
	if err != nil {
		return "task " + name + " succeeded", err
	}
	if err := r.waitHTTPS(ctx, "https://"+web+"/", "Hostname", 3*time.Minute); err != nil {
		return "task " + name + " succeeded; web restarted", err
	}
	return "task " + name + " succeeded; web restarted and answers", nil
}

func (r *runner) checkBuild(ctx context.Context, name string, since time.Time, host string) (string, error) {
	err := r.waitFor(ctx, 15*time.Minute, func(ctx context.Context) (bool, error) {
		b, err := r.console.build(ctx, project, name)
		if err != nil {
			return false, err
		}
		switch b.Phase {
		case "succeeded":
			return true, nil
		case "failed", "cancelled":
			return true, fmt.Errorf("build %s %s: %s", name, b.Phase, b.StatusMessage)
		}
		return false, fmt.Errorf("build %s is %s", name, b.Phase)
	})
	if err != nil {
		return "", err
	}
	built := r.now().Sub(since)
	if err := r.waitHTTPS(ctx, "https://"+host+"/", "Hostname", 10*time.Minute); err != nil {
		return "build succeeded in " + fmtDuration(built), err
	}
	return fmt.Sprintf("%s built in %s; https://%s answered %s after Build now", r.cfg.GitRepo, fmtDuration(built), host, fmtDuration(r.now().Sub(since))), nil
}

func (r *runner) checkLogs(ctx context.Context, marker string) (string, error) {
	var n int
	err := r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		entries, err := r.console.searchLogs(ctx, project, marker)
		if err != nil {
			return false, err
		}
		n = 0
		for _, e := range entries {
			if strings.Contains(e.Line, marker) {
				n++
			}
		}
		if n == 0 {
			return false, errors.New("the task's marker is not in the logs yet")
		}
		return true, nil
	})
	return fmt.Sprintf("%d line(s) with the task's marker", n), err
}

func (r *runner) checkMetrics(ctx context.Context) (string, error) {
	q := `kwerft:container_memory_working_set_bytes{namespace="` + project + `"}`
	var n int
	err := r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		var err error
		if n, err = r.console.metricSeries(ctx, q); err != nil {
			return false, err
		}
		if n == 0 {
			return false, errors.New("no series yet")
		}
		return true, nil
	})
	return fmt.Sprintf("%d series for %s", n, q), err
}

func (r *runner) checkAlert(ctx context.Context, since time.Time) (string, error) {
	var found alert
	err := r.waitFor(ctx, 8*time.Minute, func(ctx context.Context) (bool, error) {
		list, err := r.console.alerts(ctx)
		if err != nil {
			return false, err
		}
		for _, a := range list {
			if a.Project == project && a.App == "crash" && a.State == "firing" && (a.Rule == "crash-looping" || a.Rule == "restarts") {
				found = a
				return true, nil
			}
		}
		return false, errors.New("no crash alert for app crash yet")
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s firing %s after the app was created: %s", found.Rule, fmtDuration(r.now().Sub(since)), found.Summary), nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
