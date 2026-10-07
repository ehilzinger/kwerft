// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Shells in the pod of a running Task share everything with App shells
// (api_shell.go); these tests cover what differs: which pods qualify, and
// what the recording and the audit log say.

const taskShellPath = "/api/v1/projects/shop/tasks/nightly/pods/nightly-x1/shell"

// taskPod is a pod of Task run task, as the Task reconciler's Job makes it.
func taskPod(name, task string, running bool) corev1.Pod {
	p := appPod(name, "", time.Now(), running)
	p.Labels = map[string]string{controllers.LabelTask: task, controllers.LabelProject: "shop"}
	p.Spec.Containers[0].Name = "main"
	if running {
		p.Status.ContainerStatuses[0].Name = "main"
	}
	return p
}

func TestTaskShellHandshakeRequiresSessionAndSameOrigin(t *testing.T) {
	fake := newFakePods(taskPod("nightly-x1", "nightly", true))
	fake.exec = func(ctx context.Context, _ *corev1.PodExecOptions, _ execStreams) error { <-ctx.Done(); return nil }
	pe := newPodEnv(t, fake)

	anon := &podEnv{env: pe.newClient(), fake: fake, pods: pe.pods}
	if _, res, err := anon.dial(t, taskShellPath, originOf(pe)); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a session: %v %v, want 401", res, err)
	}
	if _, res, err := pe.dial(t, taskShellPath, http.Header{"Origin": {"https://evil.example"}}); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin: %v %v, want 403", res, err)
	}
	u, _ := url.Parse(pe.srv.URL)
	if _, res, err := pe.dial(t, taskShellPath, http.Header{"Origin": {"http://" + u.Hostname() + ".evil.example:" + u.Port()}}); err == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("look-alike origin: %v %v, want 403", res, err)
	}
	if _, res, err := pe.dial(t, taskShellPath, nil); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("without Origin: %v %v, want 403", res, err)
	}
	if code, _ := pe.raw(t, "GET", taskShellPath, ""); code != http.StatusBadRequest {
		t.Errorf("plain GET: %d, want 400", code)
	}
	if entries, _ := os.ReadDir(pe.dir); len(entries) != 0 {
		t.Errorf("refused handshakes left recordings: %v", entries)
	}
	conn, _, err := pe.dial(t, taskShellPath, originOf(pe))
	if err != nil {
		t.Fatalf("same origin: %v", err)
	}
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	if ev := c.event(&out); ev["type"] != "started" || ev["container"] != "main" {
		t.Errorf("started = %v", ev)
	}
	conn.Close()
}

func TestTaskShellRefusals(t *testing.T) {
	finishedPod := taskPod("nightly-x0", "nightly", true)
	finishedPod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}
	fake := newFakePods(
		taskPod("nightly-x1", "nightly", true),
		finishedPod,
		taskPod("backup-y1", "backup", true),
		taskPod("done-z1", "done", true),
		appPod("web-a", "web", time.Now(), true),
	)
	fake.taskPhases = map[string]kwerftv1.TaskPhase{"done": kwerftv1.TaskSucceeded}
	fake.exec = func(context.Context, *corev1.PodExecOptions, execStreams) error {
		t.Error("exec ran")
		return nil
	}
	pe := newPodEnv(t, fake)
	viewer := &podEnv{env: pe.asRole(t, store.RoleViewer), pods: pe.pods}

	open := func(who *podEnv, path, want string) {
		t.Helper()
		conn, _, err := who.dial(t, path, originOf(pe))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer conn.Close()
		c := &shellClient{t: t, conn: conn}
		var out bytes.Buffer
		if ev := c.event(&out); ev["type"] != "error" || !strings.HasPrefix(ev["message"].(string), want) {
			t.Errorf("%s: %v, want error %q", path, ev, want)
		}
	}
	base := "/api/v1/projects/shop/tasks/"
	// A pod of another run, and an App's replica, are not this run's pod.
	open(pe, base+"nightly/pods/backup-y1/shell", `Pod "backup-y1" is not part of run "nightly".`)
	open(pe, base+"nightly/pods/web-a/shell", `Pod "web-a" is not part of run "nightly".`)
	// Nor is a Task's pod one of an App's replicas.
	open(pe, "/api/v1/projects/shop/apps/web/pods/nightly-x1/shell", `Replica "nightly-x1" is not part of app "web".`)
	open(pe, base+"nightly/pods/gone/shell", `Pod "gone" of run "nightly" not found.`)
	open(pe, base+"missing/pods/nightly-x1/shell", `Task "missing" in project "shop" not found.`)
	// A finished run, and a pod whose command has ended (an earlier attempt).
	open(pe, base+"done/pods/done-z1/shell", `Run "done" has finished (succeeded)`)
	open(pe, base+"nightly/pods/nightly-x0/shell", "Cannot open a shell in main: the run's command has finished (exit code 0).")
	// Roles without exec.
	fake.denied["create/exec"] = true
	open(viewer, taskShellPath, "Your role does not allow opening a shell.")
	pe.waitIdle(t)

	entries, _ := pe.store.RecentAudit(context.Background(), 10)
	if !slices.ContainsFunc(entries, func(e store.AuditEntry) bool {
		return e.Action == "pod.exec.denied" && e.Actor == "viewer@example.com" && e.Target == "shop/nightly-x1"
	}) {
		t.Errorf("audit lacks the denied shell: %+v", entries)
	}
	if files, _ := os.ReadDir(pe.dir); len(files) != 0 {
		t.Errorf("refused sessions left recordings: %v", files)
	}
}

func TestTaskShellIsRecordedWithTheTask(t *testing.T) {
	fake := newFakePods(taskPod("nightly-x1", "nightly", true))
	fake.exec = echoShell
	pe := newPodEnv(t, fake)

	conn, _, err := pe.dial(t, taskShellPath+"?shell=sh", originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	started := c.event(&out)
	if started["type"] != "started" || started["pod"] != "nightly-x1" || started["container"] != "main" || started["shell"] != "sh" {
		t.Fatalf("started = %v", started)
	}
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("ls /data\n"))
	c.waitOutput(&out, "LS /DATA")
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("exit\n"))
	if exit := c.event(&out); exit["type"] != "exit" || exit["reason"] != "exited" {
		t.Errorf("exit = %v", exit)
	}
	pe.waitIdle(t)

	id := started["recording"].(string)
	raw, _ := os.ReadFile(filepath.Join(pe.dir, id+".json"))
	var m recordingMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.Kind != "task" || m.Task != "nightly" || m.App != "" ||
		m.Pod != "nightly-x1" || m.Container != "main" || m.Ended == nil {
		t.Errorf("sidecar = %s (%v)", raw, err)
	}
	if strings.Contains(string(raw), `"app"`) {
		t.Errorf("a task shell's sidecar names an app: %s", raw)
	}
	cast, _ := os.ReadFile(filepath.Join(pe.dir, id+".cast"))
	if !strings.Contains(string(cast), "LS /DATA") {
		t.Errorf("recording lacks the output: %s", cast)
	}

	entries, _ := pe.store.RecentAudit(context.Background(), 10)
	if !slices.ContainsFunc(entries, func(e store.AuditEntry) bool {
		return e.Action == "pod.exec" && e.Target == "shop/nightly-x1" && strings.HasSuffix(e.Detail, "recording "+id+", task nightly")
	}) || !slices.ContainsFunc(entries, func(e store.AuditEntry) bool {
		return e.Action == "pod.exec.end" && e.Target == "shop/nightly-x1"
	}) {
		t.Errorf("audit = %+v", entries)
	}

	// The list says what kind of shell it was.
	var list []recordingMeta
	_, body := pe.raw(t, "GET", "/api/v1/recordings", "")
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) != 1 || list[0].Kind != "task" || list[0].Task != "nightly" {
		t.Errorf("list = %s", body)
	}
}

func TestDebugShellInATaskPod(t *testing.T) {
	pod := taskPod("nightly-x1", "nightly", true)
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](1000), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	fake := newFakePods(pod)
	var ranIn string
	fake.exec = func(ctx context.Context, opts *corev1.PodExecOptions, s execStreams) error {
		ranIn = opts.Container
		return echoShell(ctx, opts, s)
	}
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.shell.debugPoll = 10 * time.Millisecond })

	conn, _, err := pe.dial(t, taskShellPath+"?shell=debug", originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	if ev := c.event(&out); ev["type"] != "status" {
		t.Errorf("first event = %v, want a status about the toolbox", ev)
	}
	started := c.event(&out)
	if started["type"] != "started" || started["shell"] != "debug" || started["container"] != "main" {
		t.Fatalf("started = %v", started)
	}
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("exit\n"))
	c.event(&out)
	pe.waitIdle(t)

	ecs := fake.ephemeral("nightly-x1")
	if len(ecs) != 1 || ecs[0].TargetContainerName != "main" || ranIn != ecs[0].Name {
		t.Fatalf("toolbox %+v, shell ran in %q", ecs, ranIn)
	}
	if sc := ecs[0].SecurityContext; *sc.RunAsUser != 1000 || len(sc.Capabilities.Drop) != 1 {
		t.Errorf("toolbox security context %+v does not mirror the run's container", sc)
	}
	id := started["recording"].(string)
	raw, _ := os.ReadFile(filepath.Join(pe.dir, id+".json"))
	var m recordingMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.Kind != "task" || m.Task != "nightly" || m.Shell != "debug" || m.DebugContainer != ecs[0].Name {
		t.Errorf("sidecar = %s (%v)", raw, err)
	}

	// A finished run gets no toolbox either.
	fake.taskPhases = map[string]kwerftv1.TaskPhase{"nightly": kwerftv1.TaskFailed}
	conn2, _, err := pe.dial(t, taskShellPath+"?shell=debug", originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	c2 := &shellClient{t: t, conn: conn2}
	if ev := c2.event(&out); ev["type"] != "error" || !strings.Contains(ev["message"].(string), "has finished (failed)") {
		t.Errorf("finished run: %v", ev)
	}
	if n := len(fake.ephemeral("nightly-x1")); n != 1 {
		t.Errorf("%d toolboxes after a refused debug shell, want 1", n)
	}
}

// ---- the recordings list ------------------------------------------------------

func TestRecordingsListFiltersAndMarksLiveSessions(t *testing.T) {
	pe := newPodEnv(t, newFakePods())
	rc := pe.pods.rec
	base := time.Now().Add(-time.Hour)
	add := func(i int, m recordingMeta) string {
		m.Started = base.Add(time.Duration(i) * time.Minute)
		rec, err := rc.start(m, 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		_ = rec.output([]byte("hello"), m.Started.Add(time.Second))
		if err := rec.finish(m.Started.Add(2*time.Second), "exited", nil); err != nil {
			t.Fatal(err)
		}
		return rec.meta.ID
	}
	a := add(0, recordingMeta{User: "ada@example.com", Project: "shop", Kind: "app", App: "web", Pod: "web-a", Container: "app", Shell: "bash"})
	b := add(1, recordingMeta{User: "bob@example.com", Project: "shop", Kind: "task", Task: "nightly", Pod: "nightly-x1", Container: "main", Shell: "sh"})
	c := add(2, recordingMeta{User: "ada@example.com", Project: "blog", Kind: "app", App: "ghost", Pod: "ghost-a", Container: "app", Shell: "debug"})
	// A sidecar written before Task shells existed: no kind.
	old := add(3, recordingMeta{User: "bob@example.com", Project: "blog", App: "ghost", Pod: "ghost-b", Container: "app", Shell: "sh"})
	raw, _ := os.ReadFile(filepath.Join(pe.dir, old+".json"))
	_ = os.WriteFile(filepath.Join(pe.dir, old+".json"), bytes.Replace(raw, []byte(`"kind": "",`), nil, 1), 0o600)
	// A session still running.
	liveRec, err := rc.start(recordingMeta{User: "ada@example.com", Project: "shop", Kind: "app", App: "web", Pod: "web-b", Container: "app", Shell: "sh", Started: base.Add(4 * time.Minute)}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	live, running := liveRec.meta.ID, true

	ids := func(query string) []string {
		t.Helper()
		code, body := pe.raw(t, "GET", "/api/v1/recordings"+query, "")
		var list []recordingMeta
		if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil {
			t.Fatalf("list%s: %d %s", query, code, body)
		}
		var out []string
		for _, m := range list {
			out = append(out, m.ID)
			if m.ID == old && m.Kind != "app" {
				t.Errorf("an old sidecar has kind %q, want app", m.Kind)
			}
			if m.ID == live && running && (!m.Live || m.Ended != nil) {
				t.Errorf("the running session is not marked live: %+v", m)
			}
			if m.ID != live && m.Live {
				t.Errorf("an ended session is marked live: %+v", m)
			}
		}
		return out
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{live, old, c, b, a}}, // newest first
		{"?user=ada@example.com", []string{live, c, a}},
		{"?user=ADA@example.com&project=shop", []string{live, a}},
		{"?project=blog", []string{old, c}},
		{"?task=nightly", []string{b}},
		{"?app=ghost&user=bob@example.com", []string{old}},
		{"?user=nobody@example.com", nil},
	} {
		if got := ids(tc.query); !slices.Equal(got, tc.want) {
			t.Errorf("list%s = %v, want %v", tc.query, got, tc.want)
		}
	}
	_ = liveRec.finish(time.Now(), "exited", nil)
	running = false
	if got := ids("?user=ada@example.com&project=shop"); len(got) != 2 {
		t.Errorf("after the session ended: %v", got)
	}

	// Playback is audited apart from downloads.
	if code, cast := pe.raw(t, "GET", "/api/v1/recordings/"+b+"?play=1", ""); code != http.StatusOK || !strings.Contains(cast, "hello") {
		t.Errorf("play: %d %q", code, cast)
	}
	entries, _ := pe.store.RecentAudit(context.Background(), 5)
	if !slices.ContainsFunc(entries, func(e store.AuditEntry) bool { return e.Action == "recording.play" && e.Target == b }) {
		t.Errorf("playback not audited: %+v", entries)
	}

	// Developers and viewers see none of it, filtered or not.
	for _, role := range []string{store.RoleDeveloper, store.RoleViewer} {
		other := pe.asRole(t, role)
		for _, path := range []string{"/api/v1/recordings?user=" + role + "@example.com", "/api/v1/recordings/" + b + "?play=1"} {
			if code, _ := other.raw(t, "GET", path, ""); code != http.StatusForbidden {
				t.Errorf("%s: %s = %d, want 403", role, path, code)
			}
		}
	}
}
