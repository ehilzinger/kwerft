package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/ehilzinger/kwerft/internal/store"
)

// addDebugContainer adds the toolbox to the fake pod; debugState decides how
// its status looks (running unless a test says otherwise).
func (f *fakePods) addDebugContainer(_ context.Context, ns, name string, ec corev1.EphemeralContainer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.pods {
		p := &f.pods[i]
		if p.Namespace != ns || p.Name != name {
			continue
		}
		p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, ec)
		state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		if f.debugState != nil {
			state = *f.debugState
		}
		p.Status.EphemeralContainerStatuses = append(p.Status.EphemeralContainerStatuses, corev1.ContainerStatus{Name: ec.Name, State: state})
		return nil
	}
	return errors.New("no such pod")
}

func (f *fakePods) ephemeral(name string) []corev1.EphemeralContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pods {
		if p.Name == name {
			return p.Spec.EphemeralContainers
		}
	}
	return nil
}

func TestShellWithoutShellBinarySaysSo(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	fake.exec = func(context.Context, *corev1.PodExecOptions, execStreams) error {
		// What containerd/runc report for an image built FROM scratch.
		return errors.New(`Internal error occurred: error executing command in container: failed to exec in container: failed to start exec "cc38": OCI runtime exec failed: exec failed: unable to start container process: exec: "sh": executable file not found in $PATH`)
	}
	pe := newPodEnv(t, fake)
	conn, _, err := pe.dial(t, shellPath, originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	if ev := c.event(&out); ev["type"] != "started" {
		t.Fatalf("started = %v", ev)
	}
	ev := c.event(&out)
	if ev["type"] != "exit" || ev["reason"] != "noShell" || !strings.Contains(ev["message"].(string), "debug shell") {
		t.Errorf("exit = %v, want reason noShell pointing to the debug shell", ev)
	}
}

func TestDebugShellAddsAToolboxAndRecordsIt(t *testing.T) {
	// A pod as the restricted level requires it: non-root, all capabilities dropped.
	pod := appPod("web-a", "web", time.Now(), true)
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)}
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	fake := newFakePods(pod)
	var ranIn string
	fake.exec = func(ctx context.Context, opts *corev1.PodExecOptions, s execStreams) error {
		ranIn = opts.Container
		return echoShell(ctx, opts, s)
	}
	pe := newPodEnv(t, fake, func(p *podsAPI) { p.shell.debugPoll = 10 * time.Millisecond })

	conn, _, err := pe.dial(t, shellPath+"?shell=debug", originOf(pe))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := &shellClient{t: t, conn: conn}
	var out bytes.Buffer
	if ev := c.event(&out); ev["type"] != "status" || !strings.Contains(ev["message"].(string), DefaultDebugImage) {
		t.Errorf("first event = %v, want a status naming the toolbox image", ev)
	}
	started := c.event(&out)
	if started["type"] != "started" || started["shell"] != "debug" || started["container"] != "app" {
		t.Fatalf("started = %v", started)
	}
	_ = conn.WriteMessage(2, []byte("exit\n"))
	c.event(&out)
	pe.waitIdle(t)

	ecs := fake.ephemeral("web-a")
	if len(ecs) != 1 {
		t.Fatalf("ephemeral containers = %+v", ecs)
	}
	ec := ecs[0]
	if !strings.HasPrefix(ec.Name, debugPrefix) || ec.TargetContainerName != "app" || ec.Image != DefaultDebugImage || ranIn != ec.Name {
		t.Errorf("toolbox %s (target %s, image %s), shell ran in %q", ec.Name, ec.TargetContainerName, ec.Image, ranIn)
	}
	sc := ec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65534 ||
		*sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("security context %+v does not satisfy the restricted level", sc)
	}

	id := started["recording"].(string)
	var m recordingMeta
	raw, _ := os.ReadFile(filepath.Join(pe.dir, id+".json"))
	if err := json.Unmarshal(raw, &m); err != nil || m.Shell != "debug" || m.DebugContainer != ec.Name {
		t.Errorf("sidecar = %s (%v)", raw, err)
	}
	entries, _ := pe.store.RecentAudit(context.Background(), 10)
	if !slices.ContainsFunc(entries, func(e store.AuditEntry) bool {
		return e.Action == "pod.exec" && strings.Contains(e.Detail, "debug via "+ec.Name)
	}) {
		t.Errorf("audit lacks the debug shell: %+v", entries)
	}
}

func TestDebugShellRefusals(t *testing.T) {
	fake := newFakePods(appPod("web-a", "web", time.Now(), true))
	fake.exec = func(context.Context, *corev1.PodExecOptions, execStreams) error {
		t.Error("exec ran")
		return nil
	}
	pe := newPodEnv(t, fake, func(p *podsAPI) {
		p.shell.debugPoll, p.shell.debugStart = 10*time.Millisecond, 200*time.Millisecond
	})
	open := func(want string) {
		t.Helper()
		conn, _, err := pe.dial(t, shellPath+"?shell=debug", originOf(pe))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		c := &shellClient{t: t, conn: conn}
		var out bytes.Buffer
		ev := c.event(&out)
		for ev["type"] == "status" {
			ev = c.event(&out)
		}
		if ev["type"] != "error" || !strings.Contains(ev["message"].(string), want) {
			t.Errorf("got %v, want error containing %q", ev, want)
		}
	}

	fake.denied["update/ephemeralcontainers"] = true
	open("Your role does not allow debug shells.")
	fake.denied["update/ephemeralcontainers"] = false

	fake.debugState = &corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"}}
	open("cannot start (ImagePullBackOff)")

	fake.debugState = &corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	open("did not start within")
	pe.waitIdle(t)
}

func TestDebugContainerMirrorsTheTargetUser(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)},
			Containers: []corev1.Container{
				{Name: "app", SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](2000), RunAsGroup: ptr.To[int64](3000)}},
				{Name: "side"},
			},
		},
	}
	if sc := debugContainer(pod, "app", "img", time.Hour).SecurityContext; *sc.RunAsUser != 2000 || *sc.RunAsGroup != 3000 {
		t.Errorf("app: user %v group %v, want the container's 2000/3000", *sc.RunAsUser, *sc.RunAsGroup)
	}
	if sc := debugContainer(pod, "side", "img", time.Hour).SecurityContext; *sc.RunAsUser != 1000 || sc.RunAsGroup != nil {
		t.Errorf("side: user %v group %v, want the pod's 1000", *sc.RunAsUser, sc.RunAsGroup)
	}
	ec := debugContainer(&corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}, "app", "img", time.Hour)
	if sc := ec.SecurityContext; sc.RunAsUser != nil || len(sc.Capabilities.Drop) != 0 || len(sc.Capabilities.Add) != 0 || ec.Command[1] != "3660" {
		// Same capabilities as the app (default set), so /proc/<pid>/root is readable.
		t.Errorf("no settings: %+v, command %v", sc, ec.Command)
	}
}
