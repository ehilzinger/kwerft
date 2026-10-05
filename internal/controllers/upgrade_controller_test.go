package controllers

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The Upgrade reconciler against the test API server, called pass by pass
// (it is not registered with the shared manager). The preflight reads a
// fake API (upgrade_preflight_test.go); the queue, Jobs, finalizers and
// retention are real.

const testRunnerImage = "ghcr.io/ehilzinger/kwerft@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type upgradeEnv struct {
	r     *UpgradeReconciler
	snap  *fakeSnapshotter
	clock *offsetClock
}

func newUpgradeEnv(t *testing.T) *upgradeEnv {
	t.Helper()
	requireEnvtest(t)
	e := &upgradeEnv{snap: &fakeSnapshotter{}, clock: &offsetClock{}}
	e.r = &UpgradeReconciler{
		Client: k8s, APIReader: k8s, Namespace: GatewayNamespace,
		Checks: testChecks(t), Database: e.snap, DataDir: t.TempDir(),
		RunnerImage: func(context.Context) (string, error) { return testRunnerImage, nil },
		Now:         e.clock.Now,
	}
	t.Cleanup(func() { cleanUpgrades(t) })
	return e
}

// cleanUpgrades removes every Upgrade (finalizers too) and runner Job:
// they are cluster-wide and would hold the next test's queue and pools.
func cleanUpgrades(t *testing.T) {
	ctx := context.Background()
	var list kwerftv1.UpgradeList
	if err := k8s.List(ctx, &list); err != nil {
		t.Error(err)
		return
	}
	for i := range list.Items {
		u := &list.Items[i]
		if len(u.Finalizers) > 0 {
			u.Finalizers = nil
			_ = k8s.Update(ctx, u)
		}
		_ = k8s.Delete(ctx, u)
	}
	_ = k8s.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(GatewayNamespace), client.HasLabels{upgrades.LabelUpgrade},
		client.PropagationPolicy(metav1.DeletePropagationBackground))
	_ = k8s.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(GatewayNamespace), client.HasLabels{upgrades.LabelUpgrade})
}

func createUpgrade(t *testing.T, name, version string, annotations map[string]string) *kwerftv1.Upgrade {
	t.Helper()
	u := &kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec: kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: version}}
	if strings.HasPrefix(version, "v") {
		u.Spec.Component = kwerftv1.UpgradeKubernetes
	}
	if err := k8s.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func getUpgrade(t *testing.T, name string) *kwerftv1.Upgrade {
	t.Helper()
	var u kwerftv1.Upgrade
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &u); err != nil {
		t.Fatal(err)
	}
	return &u
}

// settle reconciles until the reconciler stops asking to come back at once.
func (e *upgradeEnv) settle(t *testing.T, name string) (*kwerftv1.Upgrade, ctrl.Result) {
	t.Helper()
	var res ctrl.Result
	for range 10 {
		var err error
		res, err = e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
		if err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
		if !res.Requeue {
			break
		}
	}
	var u kwerftv1.Upgrade
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &u); apierrors.IsNotFound(err) {
		return nil, res
	} else if err != nil {
		t.Fatal(err)
	}
	return &u, res
}

// setUpgradePhase plays the runner.
func setUpgradePhase(t *testing.T, name string, phase kwerftv1.UpgradePhase) {
	t.Helper()
	u := getUpgrade(t, name)
	u.Status.Phase = phase
	if upgrades.Finished(phase) {
		u.Status.FinishedAt = &metav1.Time{Time: time.Now()}
	}
	if err := k8s.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func runnerJob(t *testing.T, upgrade string) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: GatewayNamespace, Name: RunnerJobName(upgrade)}, &job)
	if apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return &job
}

func TestUpgradeStartsTheRunner(t *testing.T) {
	e := newUpgradeEnv(t)
	e.r.InstallBaseURL = "http://install.test/main"
	createUpgrade(t, "kwerft-0.6.0-a", "0.6.0", map[string]string{upgrades.AnnotationFault: upgrades.FaultVerify})
	u, _ := e.settle(t, "kwerft-0.6.0-a")

	if u.Status.Phase != kwerftv1.UpgradeBackingUp || u.Status.StartedAt == nil || u.Status.From == nil || u.Status.From.Kwerft != "0.5.0" {
		t.Fatalf("status = %+v", u.Status)
	}
	if b := Blocked(u.Status.Preflight); len(b) > 0 || len(u.Status.Preflight) < 9 {
		t.Errorf("preflight = %+v", u.Status.Preflight)
	}
	if u.Status.Backup == nil || !strings.HasSuffix(u.Status.Backup.Database, "/backups/pre-kwerft-0.6.0-a.db") {
		t.Fatalf("backup = %+v", u.Status.Backup)
	}
	if _, err := os.Stat(u.Status.Backup.Database); err != nil {
		t.Errorf("database copy: %v", err)
	}
	if !slices.Contains(u.Finalizers, upgradeFinalizer) {
		t.Error("no finalizer while the runner may run")
	}
	job := runnerJob(t, "kwerft-0.6.0-a")
	if job == nil {
		t.Fatal("no runner Job")
	}
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]
	if c.Image != testRunnerImage || !pod.HostNetwork || pod.ServiceAccountName != upgrades.RunnerServiceAccount ||
		pod.NodeSelector[kwerftv1.LabelInstaller] != "true" || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("runner pod = %+v", pod)
	}
	wantArgs := []string{"upgrade-runner", "--upgrade=kwerft-0.6.0-a", "--host-root=/host", "--work-dir=/work/kwerft-0.6.0-a",
		"--host-work-dir=/var/lib/kwerft/upgrade/kwerft-0.6.0-a", "--install-base-url=http://install.test/main"}
	if !slices.Equal(c.Args, wantArgs) {
		t.Errorf("args = %v, want %v (no fault without --upgrade-faults)", c.Args, wantArgs)
	}
	if caps := c.SecurityContext.Capabilities; len(caps.Add) != 1 || caps.Add[0] != "SYS_CHROOT" || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("security context = %+v", c.SecurityContext)
	}
	if ref := metav1.GetControllerOf(job); ref == nil || ref.Kind != "Upgrade" || ref.UID != u.UID {
		t.Errorf("owner = %+v", ref)
	}
	if n := e.snap.n.Load(); n != 1 {
		t.Errorf("%d database copies", n)
	}

	// Deleting it now does not pull the runner: it finishes first.
	if err := k8s.Delete(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	setUpgradePhase(t, "kwerft-0.6.0-a", kwerftv1.UpgradeRunning)
	if u, _ := e.settle(t, "kwerft-0.6.0-a"); u == nil || runnerJob(t, "kwerft-0.6.0-a") == nil {
		t.Fatal("a running upgrade went away")
	}
	setUpgradePhase(t, "kwerft-0.6.0-a", kwerftv1.UpgradeSucceeded)
	if u, _ := e.settle(t, "kwerft-0.6.0-a"); u != nil {
		t.Errorf("a finished, deleted upgrade stays: %+v", u.Finalizers)
	}
}

func TestUpgradePassesFaultsOnlyWhenAllowed(t *testing.T) {
	e := newUpgradeEnv(t)
	e.r.Faults = true
	createUpgrade(t, "kwerft-0.6.0-f", "0.6.0", map[string]string{upgrades.AnnotationFault: upgrades.FaultInstall})
	e.settle(t, "kwerft-0.6.0-f")
	job := runnerJob(t, "kwerft-0.6.0-f")
	if job == nil || !slices.Contains(job.Spec.Template.Spec.Containers[0].Args, "--fault=install") {
		t.Fatalf("job = %+v", job)
	}
}

func TestUpgradeQueue(t *testing.T) {
	e := newUpgradeEnv(t)
	createUpgrade(t, "kwerft-0.6.0-a", "0.6.0", nil)
	createUpgrade(t, "kwerft-0.6.0-b", "0.6.0", nil)

	// The newer one waits behind the older, even when it comes first.
	if b, _ := e.settle(t, "kwerft-0.6.0-b"); b.Status.Phase != kwerftv1.UpgradeQueued || !strings.Contains(b.Status.Message, "behind kwerft-0.6.0-a") {
		t.Fatalf("b = %s %q", b.Status.Phase, b.Status.Message)
	}
	if a, _ := e.settle(t, "kwerft-0.6.0-a"); a.Status.Phase != kwerftv1.UpgradeBackingUp {
		t.Fatalf("a = %s %q", a.Status.Phase, a.Status.Message)
	}
	if b, _ := e.settle(t, "kwerft-0.6.0-b"); b.Status.Phase != kwerftv1.UpgradeQueued || b.Status.Message != "Waiting for kwerft-0.6.0-a to finish." {
		t.Fatalf("b = %s %q", b.Status.Phase, b.Status.Message)
	}
	// The queue's map function names the waiting one.
	reqs := e.r.waiting(context.Background(), nil)
	if len(reqs) != 1 || reqs[0].Name != "kwerft-0.6.0-b" {
		t.Errorf("waiting = %v", reqs)
	}
	setUpgradePhase(t, "kwerft-0.6.0-a", kwerftv1.UpgradeRolledBack)
	e.settle(t, "kwerft-0.6.0-a")
	if b, _ := e.settle(t, "kwerft-0.6.0-b"); b.Status.Phase != kwerftv1.UpgradeBackingUp {
		t.Fatalf("b after a = %s %q", b.Status.Phase, b.Status.Message)
	}
}

func TestUpgradeCancel(t *testing.T) {
	e := newUpgradeEnv(t)
	ctx := context.Background()
	createUpgrade(t, "kwerft-0.6.0-a", "0.6.0", nil)
	e.settle(t, "kwerft-0.6.0-a")
	if runnerJob(t, "kwerft-0.6.0-a") == nil {
		t.Fatal("no runner Job")
	}
	createUpgrade(t, "kwerft-0.6.0-b", "0.6.0", map[string]string{kwerftv1.AnnotationCancelRequested: "bob@example.com"})
	if b, _ := e.settle(t, "kwerft-0.6.0-b"); b.Status.Phase != kwerftv1.UpgradeCancelled || !strings.Contains(b.Status.Message, "bob@example.com") {
		t.Errorf("queued b = %s %q", b.Status.Phase, b.Status.Message)
	}

	// In Backup (the runner has not started the installer): the Job goes.
	a := getUpgrade(t, "kwerft-0.6.0-a")
	a.Annotations = map[string]string{kwerftv1.AnnotationCancelRequested: "alice@example.com"}
	if err := k8s.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	a, _ = e.settle(t, "kwerft-0.6.0-a")
	if a.Status.Phase != kwerftv1.UpgradeCancelled || a.Status.FinishedAt == nil || slices.Contains(a.Finalizers, upgradeFinalizer) {
		t.Errorf("a = %s %v", a.Status.Phase, a.Finalizers)
	}
	if job := runnerJob(t, "kwerft-0.6.0-a"); job != nil && job.DeletionTimestamp.IsZero() {
		t.Error("the runner Job of a cancelled upgrade stays")
	}

	// Once the runner owns it, a cancel is ignored.
	createUpgrade(t, "kwerft-0.6.0-c", "0.6.0", nil)
	e.settle(t, "kwerft-0.6.0-c")
	setUpgradePhase(t, "kwerft-0.6.0-c", kwerftv1.UpgradeRunning)
	c := getUpgrade(t, "kwerft-0.6.0-c")
	c.Annotations = map[string]string{kwerftv1.AnnotationCancelRequested: "alice@example.com"}
	if err := k8s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if c, _ = e.settle(t, "kwerft-0.6.0-c"); c.Status.Phase != kwerftv1.UpgradeRunning {
		t.Errorf("c = %s", c.Status.Phase)
	}
}

func TestUpgradeRecordsARunnerThatGaveUp(t *testing.T) {
	e := newUpgradeEnv(t)
	ctx := context.Background()
	createUpgrade(t, "kwerft-0.6.0-a", "0.6.0", nil)
	e.settle(t, "kwerft-0.6.0-a")
	setUpgradePhase(t, "kwerft-0.6.0-a", kwerftv1.UpgradeRunning)
	job := runnerJob(t, "kwerft-0.6.0-a")
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, LastTransitionTime: now},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, Message: "Job was active longer than specified deadline", LastTransitionTime: now},
	}
	if err := k8s.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	u, _ := e.settle(t, "kwerft-0.6.0-a")
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Timeout" || !strings.Contains(u.Status.Message, "systemctl status kwerft-upgrade-kwerft-0.6.0-a") {
		t.Errorf("status = %s %s %q", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
}

func TestUpgradePreflightFailureStartsNothing(t *testing.T) {
	e := newUpgradeEnv(t)
	e.r.Checks.Version = "0.1.0-dev"
	createUpgrade(t, "kwerft-0.6.0-a", "0.6.0", nil)
	u, _ := e.settle(t, "kwerft-0.6.0-a")
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Preflight" || !strings.Contains(u.Status.Message, "ReleaseInstall:") {
		t.Fatalf("status = %s %s %q", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
	if runnerJob(t, "kwerft-0.6.0-a") != nil || e.snap.n.Load() != 0 {
		t.Error("a failed preflight started something")
	}
}

func TestKubernetesUpgradeNeedsADriver(t *testing.T) {
	e := newUpgradeEnv(t)
	createUpgrade(t, "kubernetes-v1.37.2-k3s1-a", "v1.37.2+k3s1", nil)
	u, _ := e.settle(t, "kubernetes-v1.37.2-k3s1-a")
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Preflight" {
		t.Errorf("status = %s %q", u.Status.Phase, u.Status.Message)
	}

	// With U4's driver, the active Kubernetes upgrade is handed over.
	var seen []string
	e.r.Kubernetes = kubernetesDriver(func(_ context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
		seen = append(seen, string(u.Status.Phase))
		u.Status.Phase = kwerftv1.UpgradeSucceeded
		return ctrl.Result{}, nil
	})
	createUpgrade(t, "kubernetes-v1.37.2-k3s1-b", "v1.37.2+k3s1", nil)
	if u, _ := e.settle(t, "kubernetes-v1.37.2-k3s1-b"); u.Status.Phase != kwerftv1.UpgradeSucceeded || !slices.Equal(seen, []string{"Preflight"}) {
		t.Errorf("status = %s, driver saw %v", u.Status.Phase, seen)
	}
	if runnerJob(t, "kubernetes-v1.37.2-k3s1-b") != nil {
		t.Error("a runner Job for a Kubernetes upgrade")
	}
}

type kubernetesDriver func(context.Context, *kwerftv1.Upgrade) (ctrl.Result, error)

func (f kubernetesDriver) Reconcile(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	return f(ctx, u)
}

func TestUpgradeRetention(t *testing.T) {
	e := newUpgradeEnv(t)
	ctx := context.Background()
	for i := range KeepUpgrades + 2 {
		name := fmt.Sprintf("kwerft-0.6.0-old%02d", i)
		u := createUpgrade(t, name, "0.6.0", nil)
		setUpgradePhase(t, name, kwerftv1.UpgradeSucceeded)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: upgrades.LogConfigMapName(name), Namespace: GatewayNamespace,
			Labels: map[string]string{upgrades.LabelUpgrade: name}, OwnerReferences: []metav1.OwnerReference{upgrades.UpgradeOwner(u)}},
			Data: map[string]string{upgrades.LogKey: "log"}}
		if err := k8s.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
	}
	e.settle(t, "kwerft-0.6.0-old21")
	var list kwerftv1.UpgradeList
	if err := k8s.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != KeepUpgrades {
		t.Fatalf("%d upgrades kept, want %d", len(list.Items), KeepUpgrades)
	}
	for _, gone := range []string{"kwerft-0.6.0-old00", "kwerft-0.6.0-old01"} {
		if err := k8s.Get(ctx, client.ObjectKey{Name: gone}, &kwerftv1.Upgrade{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s: %v", gone, err)
		}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: upgrades.LogConfigMapName(gone)}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Errorf("log of %s: %v", gone, err)
		}
	}
}

func TestAutoUpdateWaitsForTheWindowAndPausesAutoPatch(t *testing.T) {
	e := newUpgradeEnv(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-30 * time.Minute)
	useSettings(t, kwerftv1.ConsoleSettingsSpec{Updates: &kwerftv1.UpdateSettings{Policy: kwerftv1.UpdatesAutoPatch,
		Window: &kwerftv1.MaintenanceWindow{Start: start.Format("15:04"), Duration: &metav1.Duration{Duration: 2 * time.Hour}}}})

	auto := map[string]string{kwerftv1.AnnotationRequestedBy: kwerftv1.RequestedByAutoUpdate}
	createUpgrade(t, "kwerft-0.5.1-auto", "0.6.0", auto)
	e.clock.Advance(3 * time.Hour) // past the window
	u, res := e.settle(t, "kwerft-0.5.1-auto")
	if u.Status.Phase != kwerftv1.UpgradeQueued || u.Status.Message != "Waiting for the maintenance window." || res.RequeueAfter < time.Hour {
		t.Fatalf("outside the window: %s %q %v", u.Status.Phase, u.Status.Message, res)
	}
	e.clock.Reset()
	if u, _ = e.settle(t, "kwerft-0.5.1-auto"); u.Status.Phase != kwerftv1.UpgradeBackingUp {
		t.Fatalf("inside the window: %s %q", u.Status.Phase, u.Status.Message)
	}

	setUpgradePhase(t, "kwerft-0.5.1-auto", kwerftv1.UpgradeRolledBack)
	eventually(t, func() error {
		u, _ := e.settle(t, "kwerft-0.5.1-auto")
		if !meta.IsStatusConditionTrue(u.Status.Conditions, ConditionAutoPatchPaused) {
			return fmt.Errorf("conditions %+v", u.Status.Conditions)
		}
		return nil
	})
	var s kwerftv1.ConsoleSettings
	if err := k8s.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Status.Updates == nil || s.Status.Updates.AutoPatchPausedBy != "kwerft-0.5.1-auto" {
		t.Errorf("updates = %+v", s.Status.Updates)
	}
}

func TestNodePoolsWaitForAnActiveUpgrade(t *testing.T) {
	requireEnvtest(t)
	t.Cleanup(func() { cleanUpgrades(t) })
	ctx := context.Background()
	createUpgrade(t, "kwerft-0.6.0-a", "0.6.0", nil)
	if name, err := activeUpgrade(ctx, k8s); err != nil || name != "" {
		t.Fatalf("new upgrade counts as active: %q %v", name, err)
	}
	setUpgradePhase(t, "kwerft-0.6.0-a", kwerftv1.UpgradeRunning)
	if name, err := activeUpgrade(ctx, k8s); err != nil || name != "kwerft-0.6.0-a" {
		t.Fatalf("active = %q %v", name, err)
	}
}
