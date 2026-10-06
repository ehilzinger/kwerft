package upgrades

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// The runner's state machine against a fake host and a fake cluster: the
// installer "runs" when the runner starts its unit, writing progress lines
// and an exit code as install.sh --progress does.

type fakeHost struct {
	mu        sync.Mutex
	dir       string // the runner's directory, where the "installer" writes progress
	free      uint64
	runs      [][]string
	helm      []string // successive `helm list` outputs; the last one repeats
	rollbackE error
	snapshotE error
	started   []Command
	unit      UnitState
	unitCalls int
	// finishAfter: Unit reports the running unit finished after this many
	// calls (resume tests).
	finishAfter int
	finishCode  int
	install     func(h *fakeHost) // the installer, run by Start
	removed     []string
	noEnv       bool // no install.env (installed before Phase 6)
}

func (h *fakeHost) Run(_ context.Context, cmd Command) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = append(h.runs, cmd.Args)
	switch {
	case cmd.Args[0] == HostHelm && cmd.Args[1] == "list":
		out := h.helm[0]
		if len(h.helm) > 1 {
			h.helm = h.helm[1:]
		}
		return out, nil
	case cmd.Args[0] == HostHelm && cmd.Args[1] == "rollback":
		if h.rollbackE != nil {
			return "Error: release kwerft failed: timed out waiting for the condition", h.rollbackE
		}
		return "Rollback was a success! Happy Helming!", nil
	case cmd.Args[0] == HostK3s:
		if h.snapshotE != nil {
			return "etcd datastore disabled", h.snapshotE
		}
		return "Snapshot pre-x saved.", nil
	}
	return "", fmt.Errorf("unexpected command %v", cmd.Args)
}

func (h *fakeHost) Start(_ context.Context, cmd Command) error {
	h.mu.Lock()
	h.started = append(h.started, cmd)
	h.unit = UnitState{Exists: true, Running: true}
	install := h.install
	h.mu.Unlock()
	if install != nil {
		install(h)
	}
	return nil
}

func (h *fakeHost) Unit(_ context.Context, name string) (UnitState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unitCalls++
	if h.finishAfter > 0 && h.unitCalls >= h.finishAfter && h.unit.Running {
		h.unit = UnitState{Exists: true, Finished: true, ExitCode: h.finishCode}
	}
	return h.unit, nil
}

func (h *fakeHost) Remove(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = append(h.removed, name)
	h.unit = UnitState{}
	return nil
}

func (h *fakeHost) FreeBytes(string) (uint64, error) { return h.free, nil }
func (h *fakeHost) Exists(p string) bool             { return p != HostInstallEnv || !h.noEnv }
func (h *fakeHost) LogTail(n int) ([]byte, error) {
	return []byte("✓ Kwerft  control plane ready\n"), nil
}

// exits makes the installer write stages and an exit code, then end.
func (h *fakeHost) exits(code int, stages ...string) {
	h.install = func(h *fakeHost) {
		var lines []string
		for _, s := range stages {
			id, state, _ := strings.Cut(s, ":")
			lines = append(lines, fmt.Sprintf(`{"id":%q,"label":%q,"state":%q,"detail":"%s detail","at":"2026-10-05T10:00:00Z"}`,
				id, strings.ToUpper(id[:1])+id[1:], state, id))
		}
		lines = append(lines, fmt.Sprintf(`{"exit":%d}`, code))
		if err := os.WriteFile(filepath.Join(h.dir, fileProgress), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			panic(err)
		}
		h.mu.Lock()
		h.unit = UnitState{Exists: true, Finished: true, ExitCode: code}
		h.mu.Unlock()
	}
}

func (h *fakeHost) commands(prog, verb string) [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out [][]string
	for _, r := range h.runs {
		if r[0] == prog && (verb == "" || r[1] == verb) {
			out = append(out, r)
		}
	}
	return out
}

type fakeCluster struct {
	mu      sync.Mutex
	u       *kwerftv1.Upgrade
	apps    map[string]int32
	verify  func(t VerifyTarget) []string
	log     []byte
	updates int
}

func (c *fakeCluster) Upgrade(context.Context) (*kwerftv1.Upgrade, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.u.DeepCopy(), nil
}

func (c *fakeCluster) UpdateStatus(_ context.Context, mutate func(*kwerftv1.Upgrade) error) (*kwerftv1.Upgrade, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.u.DeepCopy()
	if err := mutate(u); err != nil {
		return nil, err
	}
	c.u.Status = u.Status
	c.updates++
	return u, nil
}

func (c *fakeCluster) Annotate(_ context.Context, key, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.u.Annotations == nil {
		c.u.Annotations = map[string]string{}
	}
	c.u.Annotations[key] = value
	return nil
}

func (c *fakeCluster) AppReplicas(context.Context) (map[string]int32, error) { return c.apps, nil }

func (c *fakeCluster) Verify(_ context.Context, t VerifyTarget) []string {
	if c.verify == nil {
		return nil
	}
	return c.verify(t)
}

func (c *fakeCluster) WriteLog(_ context.Context, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = data
	return nil
}

type fakeFiles map[string][]byte

func (f fakeFiles) Fetch(_ context.Context, path string) ([]byte, error) {
	if b, ok := f[path]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
}

func release(version string, manifest string) fakeFiles {
	installer := []byte("#!/usr/bin/env bash\necho kwerft " + version + "\n")
	sum := sha256.Sum256(installer)
	if manifest == "" {
		manifest = `{"version":"` + version + `"}`
	}
	return fakeFiles{
		"v" + version + "/install.sh":    installer,
		"v" + version + "/SHA256SUMS":    []byte(hex.EncodeToString(sum[:]) + "  install.sh\n0000  join.sh\n"),
		"v" + version + "/manifest.json": []byte(manifest),
	}
}

const (
	helmBefore = `[{"name":"cilium","namespace":"kube-system","revision":"3"},{"name":"traefik","namespace":"traefik","revision":"5"},{"name":"kwerft","namespace":"kwerft-system","revision":"12"}]`
	helmAfter  = `[{"name":"cilium","namespace":"kube-system","revision":"4"},{"name":"traefik","namespace":"traefik","revision":"5"},{"name":"kwerft","namespace":"kwerft-system","revision":"13"},{"name":"velero","namespace":"velero","revision":1}]`
)

type runnerEnv struct {
	r       *Runner
	host    *fakeHost
	cluster *fakeCluster
}

func newRunnerEnv(t *testing.T, phase kwerftv1.UpgradePhase) *runnerEnv {
	t.Helper()
	dir := t.TempDir()
	u := &kwerftv1.Upgrade{
		ObjectMeta: metav1.ObjectMeta{Name: "kwerft-0.6.0-x7k2p"},
		Spec:       kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"},
		Status: kwerftv1.UpgradeStatus{Phase: phase, From: &kwerftv1.UpgradeVersions{Kwerft: "0.5.0", Kubernetes: "v1.37.1+k3s1"},
			Backup: &kwerftv1.UpgradeBackup{Database: "/var/lib/kwerft/backups/pre-kwerft-0.6.0-x7k2p.db"}},
	}
	host := &fakeHost{dir: dir, free: 20 << 30, helm: []string{helmBefore}}
	cluster := &fakeCluster{u: u, apps: map[string]int32{"shop/web": 2}}
	return &runnerEnv{
		host: host, cluster: cluster,
		r: &Runner{Name: u.Name, Cluster: cluster, Host: host, Files: release("0.6.0", ""),
			Dir: dir, HostDir: "/var/lib/kwerft/upgrade/" + u.Name,
			Poll: time.Millisecond, VerifyTimeout: 30 * time.Millisecond, Log: slog.New(slog.DiscardHandler)},
	}
}

func (e *runnerEnv) run(t *testing.T) *kwerftv1.Upgrade {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.r.Run(ctx); err != nil {
		t.Fatalf("runner: %v", err)
	}
	u, _ := e.cluster.Upgrade(ctx)
	return u
}

func TestRunnerUpgradesAndVerifies(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.exits(0, "preflight:ok", "system:skip", "kwerft:ok")
	e.host.helm = []string{helmBefore, helmAfter}
	var verified []string
	e.cluster.verify = func(t VerifyTarget) []string {
		verified = append(verified, t.Version)
		if t.Apps["shop/web"] != 2 {
			return []string{"no App baseline"}
		}
		return nil
	}
	u := e.run(t)

	if u.Status.Phase != kwerftv1.UpgradeSucceeded || u.Status.FinishedAt == nil {
		t.Fatalf("phase %s (%s), want Succeeded", u.Status.Phase, u.Status.Message)
	}
	b := u.Status.Backup
	if b.EtcdSnapshot != "pre-kwerft-0.6.0-x7k2p" || b.HelmRevisions["kwerft-system/kwerft"] != 12 || b.HelmRevisions["kube-system/cilium"] != 3 {
		t.Errorf("backup = %+v", b)
	}
	if b.Database == "" {
		t.Error("the controller's database copy was dropped")
	}
	if snaps := e.host.commands(HostK3s, "etcd-snapshot"); len(snaps) != 1 || !slices.Contains(snaps[0], "pre-kwerft-0.6.0-x7k2p") {
		t.Errorf("etcd snapshot commands: %v", snaps)
	}
	if len(e.host.started) != 1 {
		t.Fatalf("installer started %d times", len(e.host.started))
	}
	want := []string{e.r.HostDir + "/install.sh", "--version", "0.6.0", "--yes", "--progress", e.r.HostDir + "/progress.jsonl"}
	if got := e.host.started[0]; !slices.Equal(got.Args, want) || got.Unit != "kwerft-upgrade-kwerft-0.6.0-x7k2p" {
		t.Errorf("installer = %s %v, want %v", got.Unit, got.Args, want)
	}
	if len(u.Status.Steps) != 3 || u.Status.Steps[0].State != StepDone || u.Status.Steps[1].State != StepSkipped || u.Status.Steps[2].At == nil {
		t.Errorf("steps = %+v", u.Status.Steps)
	}
	if !slices.Equal(verified, []string{"0.6.0"}) {
		t.Errorf("verified %v", verified)
	}
	if len(e.host.commands(HostHelm, "rollback")) != 0 {
		t.Error("rolled back a successful upgrade")
	}
	if !strings.Contains(string(e.cluster.log), "control plane ready") || !slices.Contains(e.host.removed, "kwerft-upgrade-kwerft-0.6.0-x7k2p") {
		t.Errorf("log %q, removed %v", e.cluster.log, e.host.removed)
	}
	// The downloaded installer is what SHA256SUMS lists, executable.
	if fi, err := os.Stat(filepath.Join(e.r.Dir, "install.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("install.sh: %v %v", fi, err)
	}
}

func TestRunnerRollsBackAFailedInstaller(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.exits(50, "network:ok", "kwerft:fail")
	e.host.helm = []string{helmBefore, helmAfter}
	e.cluster.verify = func(t VerifyTarget) []string {
		if t.Version != "0.5.0" {
			return []string{"verified the wrong version " + t.Version}
		}
		return nil
	}
	u := e.run(t)

	if u.Status.Phase != kwerftv1.UpgradeRolledBack || u.Status.Reason != "Kwerft" {
		t.Fatalf("phase %s reason %s (%s), want RolledBack/Kwerft", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
	// Kwerft first, Cilium last; Traefik did not change; Velero is new.
	rb := e.host.commands(HostHelm, "rollback")
	if len(rb) != 2 || rb[0][2] != "kwerft" || rb[0][3] != "12" || rb[1][2] != "cilium" || rb[1][3] != "3" {
		t.Fatalf("rollbacks = %v", rb)
	}
	for _, s := range []string{"stage Kwerft (exit 50): kwerft detail", "Rolled back to 0.5.0", "velero/velero"} {
		if !strings.Contains(u.Status.Message, s) {
			t.Errorf("message %q lacks %q", u.Status.Message, s)
		}
	}
	if u.Annotations[AnnotationRestoreDatabase] != "" {
		t.Error("restored the database of a rollback-safe release")
	}
}

func TestRunnerRollsBackAFailedVerification(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.exits(0, "kwerft:ok")
	e.host.helm = []string{helmBefore, helmAfter}
	e.cluster.verify = func(t VerifyTarget) []string {
		if t.Version == "0.6.0" {
			return []string{"the console reports 0.5.0, not 0.6.0"}
		}
		return nil
	}
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeRolledBack || u.Status.Reason != "Verify" ||
		!strings.Contains(u.Status.Message, "the console reports 0.5.0, not 0.6.0") {
		t.Fatalf("phase %s reason %s (%s)", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
}

func TestRunnerFaultInjection(t *testing.T) {
	for _, fault := range []string{FaultInstall, FaultVerify} {
		t.Run(fault, func(t *testing.T) {
			e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
			e.r.Fault = fault
			e.host.exits(0, "kwerft:ok")
			e.host.helm = []string{helmBefore, helmAfter}
			u := e.run(t)
			if u.Status.Phase != kwerftv1.UpgradeRolledBack || !strings.Contains(u.Status.Message, "Fault injected") {
				t.Fatalf("phase %s (%s)", u.Status.Phase, u.Status.Message)
			}
			if len(e.host.commands(HostHelm, "rollback")) != 2 {
				t.Errorf("rollbacks: %v", e.host.commands(HostHelm, "rollback"))
			}
		})
	}
}

func TestRunnerFailsWhenTheRollbackFails(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.exits(40, "observability:fail")
	e.host.helm = []string{helmBefore, helmAfter}
	e.host.rollbackE = errors.New("exit status 1")
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Platform" {
		t.Fatalf("phase %s reason %s", u.Status.Phase, u.Status.Reason)
	}
	for _, s := range []string{"rollback did not complete", "helm rollback kwerft 12 -n kwerft-system --wait", "pre-kwerft-0.6.0-x7k2p", "pre-kwerft-0.6.0-x7k2p.db"} {
		if !strings.Contains(u.Status.Message, s) {
			t.Errorf("message %q lacks %q", u.Status.Message, s)
		}
	}
}

func TestRunnerRefusesABadChecksum(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	files := release("0.6.0", "")
	files["v0.6.0/install.sh"] = []byte("#!/bin/sh\nrm -rf /\n")
	e.r.Files = files
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Preflight" || !strings.Contains(u.Status.Message, "does not match its SHA256SUMS") {
		t.Fatalf("phase %s (%s)", u.Status.Phase, u.Status.Message)
	}
	if len(e.host.started) != 0 || len(e.host.commands(HostK3s, "")) != 0 {
		t.Error("ran something after a checksum mismatch")
	}
}

func TestRunnerChecksDiskSpaceFirst(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.free = 2 << 30
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeFailed || !strings.Contains(u.Status.Message, "Only 2.0 GiB free") {
		t.Fatalf("phase %s (%s)", u.Status.Phase, u.Status.Message)
	}
}

func TestRunnerNeedsInstallEnv(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.noEnv = true
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Preflight" || !strings.Contains(u.Status.Message, "install.env") {
		t.Fatalf("phase %s (%s)", u.Status.Phase, u.Status.Message)
	}
	if len(e.host.runs) != 0 {
		t.Errorf("ran %v", e.host.runs)
	}
}

func TestRunnerFailsBeforeAnythingWhenTheSnapshotFails(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.snapshotE = errors.New("exit status 1")
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Backup" || !strings.Contains(u.Status.Message, "etcd datastore disabled") {
		t.Fatalf("phase %s reason %s (%s)", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
	if len(e.host.started) != 0 {
		t.Error("started the installer without a snapshot")
	}
}

func TestRunnerHonoursACancelBeforeItStarts(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.cluster.u.Annotations = map[string]string{kwerftv1.AnnotationCancelRequested: "alice@example.com"}
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeCancelled || !strings.Contains(u.Status.Message, "alice@example.com") {
		t.Fatalf("phase %s (%s)", u.Status.Phase, u.Status.Message)
	}
	if len(e.host.runs) != 0 || len(e.host.started) != 0 {
		t.Errorf("ran %v", e.host.runs)
	}
}

func TestRunnerResumesARunningInstaller(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeRunning)
	// A previous runner pod started the installer and died.
	if err := os.WriteFile(filepath.Join(e.r.Dir, fileStarted), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	e.cluster.u.Status.Backup.HelmRevisions = map[string]int32{"kwerft-system/kwerft": 12}
	e.host.unit = UnitState{Exists: true, Running: true}
	e.host.finishAfter, e.host.finishCode = 4, 0
	u := e.run(t)
	if len(e.host.started) != 0 {
		t.Fatal("started the installer a second time")
	}
	if u.Status.Phase != kwerftv1.UpgradeSucceeded {
		t.Fatalf("phase %s (%s)", u.Status.Phase, u.Status.Message)
	}
}

func TestRunnerNoticesAVanishedInstaller(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeRunning)
	if err := os.WriteFile(filepath.Join(e.r.Dir, fileStarted), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	e.cluster.u.Status.Backup.HelmRevisions, _ = ParseHelmList(helmBefore)
	e.host.helm = []string{helmBefore}
	u := e.run(t)
	if strings.Contains(u.Status.Message, "Left installed") {
		t.Errorf("message %q", u.Status.Message)
	}
	if u.Status.Phase != kwerftv1.UpgradeRolledBack || u.Status.Reason != "Installer" || !strings.Contains(u.Status.Message, "no Helm release had changed") {
		t.Fatalf("phase %s reason %s (%s)", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
}

func TestRunnerStopsAnInstallerThatHangs(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.r.InstallTimeout = 20 * time.Millisecond
	e.host.helm = []string{helmBefore}
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeRolledBack || u.Status.Reason != "Timeout" {
		t.Fatalf("phase %s reason %s (%s)", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
	if !slices.Contains(e.host.removed, "kwerft-upgrade-kwerft-0.6.0-x7k2p") {
		t.Error("the hanging installer was not stopped")
	}
}

func TestRunnerRestoresTheDatabaseOfAnUnsafeRelease(t *testing.T) {
	e := newRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.r.Files = release("0.6.0", `{"version":"0.6.0","rollbackSafe":false}`)
	e.cluster.u.Spec.AcceptDataRollback = true
	e.host.exits(50, "kwerft:fail")
	e.host.helm = []string{helmBefore, helmAfter}
	u := e.run(t)
	if u.Annotations[AnnotationRestoreDatabase] != "0.5.0" {
		t.Errorf("restore annotation = %q, want the version rolled back to", u.Annotations[AnnotationRestoreDatabase])
	}
	if u.Status.Phase != kwerftv1.UpgradeRolledBack || !strings.Contains(u.Status.Message, "database was restored") {
		t.Fatalf("phase %s (%s)", u.Status.Phase, u.Status.Message)
	}
}

func TestRollbackPlanOrder(t *testing.T) {
	before := map[string]int32{"kube-system/cilium": 1, "traefik/traefik": 2, "kwerft-system/kwerft": 3,
		"velero/velero": 1, "kwerft-observability/vm": 4, "cert-manager/cert-manager": 5}
	now := map[string]int32{"kube-system/cilium": 2, "traefik/traefik": 3, "kwerft-system/kwerft": 4,
		"velero/velero": 2, "kwerft-observability/vm": 4, "cert-manager/cert-manager": 6, "system-upgrade/suc": 1}
	steps, added := RollbackPlan(before, now)
	var got []string
	for _, s := range steps {
		got = append(got, s.Release)
	}
	want := []string{"kwerft-system/kwerft", "velero/velero", "traefik/traefik", "cert-manager/cert-manager", "kube-system/cilium"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if steps[0].Revision != 3 || !slices.Equal(added, []string{"system-upgrade/suc"}) {
		t.Errorf("steps %v added %v", steps, added)
	}
}

func TestParseHelmList(t *testing.T) {
	revs, err := ParseHelmList("WARNING: kubeconfig is group-readable\n" + helmAfter)
	if err != nil {
		t.Fatal(err)
	}
	if revs["velero/velero"] != 1 || revs["kube-system/cilium"] != 4 || len(revs) != 4 {
		t.Errorf("revisions = %v", revs)
	}
	if _, err := ParseHelmList("Error: Kubernetes cluster unreachable"); err == nil {
		t.Error("no error without JSON")
	}
	// Helm 4.3's output, as on a v0.6.0-rc.3 server.
	revs, err = ParseHelmList(`[{"name":"cert-manager","namespace":"cert-manager","revision":"5","updated":"2026-10-06 08:24:59.80093093 +0000 UTC","status":"deployed","chart":"cert-manager-v1.21.2","app_version":"v1.21.2"}]`)
	if err != nil || revs["cert-manager/cert-manager"] != 5 {
		t.Errorf("helm 4: %v %v", revs, err)
	}
}

// The runner's helm list flags exist in the helm on this machine (Helm 4
// dropped --all, which failed v0.6.0-rc.3's console upgrades); skipped
// without helm.
func TestHelmListArgsExist(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("no helm")
	}
	out, err := exec.Command(helm, "list", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("helm list --help: %v: %s", err, out)
	}
	for _, a := range HelmListArgs[1:] {
		if strings.HasPrefix(a, "--") && !strings.Contains(string(out), a+" ") {
			t.Errorf("helm list has no %s", a)
		}
	}
}

func TestParseSums(t *testing.T) {
	sums := ParseSums([]byte("ABC  install.sh\ndef *join.sh\nbroken\n"))
	if sums["install.sh"] != "abc" || sums["join.sh"] != "def" || len(sums) != 2 {
		t.Errorf("sums = %v", sums)
	}
}

func kubernetesRunnerEnv(t *testing.T, phase kwerftv1.UpgradePhase) *runnerEnv {
	t.Helper()
	e := newRunnerEnv(t, phase)
	e.cluster.u.Name = "kubernetes-v1-38-1-k3s1-ab"
	e.cluster.u.Spec = kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKubernetes, Version: "v1.38.1+k3s1"}
	e.cluster.u.Status.Backup = nil
	e.r.Name = e.cluster.u.Name
	return e
}

// A Kubernetes upgrade's runner only takes the etcd snapshot; the
// controller does the rest.
func TestRunnerTakesTheSnapshotOfAKubernetesUpgrade(t *testing.T) {
	e := kubernetesRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	u := e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeBackingUp || u.Status.Backup == nil || u.Status.Backup.EtcdSnapshot != "pre-kubernetes-v1-38-1-k3s1-ab" {
		t.Fatalf("status = %s %+v", u.Status.Phase, u.Status.Backup)
	}
	want := [][]string{{HostK3s, "etcd-snapshot", "save", "--name", "pre-kubernetes-v1-38-1-k3s1-ab"}}
	if runs := e.host.runs; len(runs) != 1 || !slices.Equal(runs[0], want[0]) {
		t.Errorf("ran %v", runs)
	}
	if len(e.host.started) != 0 {
		t.Error("started an installer")
	}
	// Run again (a retried pod): nothing more.
	e.run(t)
	if len(e.host.runs) != 1 {
		t.Errorf("snapshot taken twice: %v", e.host.runs)
	}
	// The cluster restored from the snapshot: the Upgrade is in Backup
	// again without it. It is not repeated.
	e.cluster.u.Status.Backup = nil
	u = e.run(t)
	if u.Status.Phase != kwerftv1.UpgradeFailed || !strings.Contains(u.Status.Message, "restored from this upgrade's etcd snapshot") || len(e.host.runs) != 1 {
		t.Errorf("after a restore: %s %q, ran %v", u.Status.Phase, u.Status.Message, e.host.runs)
	}
}

func TestRunnerKubernetesSnapshotFailures(t *testing.T) {
	e := kubernetesRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.snapshotE = errors.New("exit status 1")
	if u := e.run(t); u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Backup" ||
		!strings.Contains(u.Status.Message, "etcd datastore disabled") || !strings.Contains(u.Status.Message, "Nothing was changed") {
		t.Errorf("snapshot failed: %s %s %q", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}

	e = kubernetesRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.host.free = 1 << 30
	if u := e.run(t); u.Status.Phase != kwerftv1.UpgradeFailed || !strings.Contains(u.Status.Message, "1.0 GiB free") || len(e.host.runs) != 0 {
		t.Errorf("disk: %s %q, ran %v", u.Status.Phase, u.Status.Message, e.host.runs)
	}

	// Cancelled, or past Backup: nothing to do.
	for _, phase := range []kwerftv1.UpgradePhase{kwerftv1.UpgradeRunning, kwerftv1.UpgradeSucceeded} {
		e = kubernetesRunnerEnv(t, phase)
		if u := e.run(t); u.Status.Phase != phase || len(e.host.runs) != 0 {
			t.Errorf("%s: %s, ran %v", phase, u.Status.Phase, e.host.runs)
		}
	}
	e = kubernetesRunnerEnv(t, kwerftv1.UpgradeBackingUp)
	e.cluster.u.Annotations = map[string]string{kwerftv1.AnnotationCancelRequested: "alice@example.com"}
	if e.run(t); len(e.host.runs) != 0 {
		t.Errorf("cancelled: ran %v", e.host.runs)
	}
}
