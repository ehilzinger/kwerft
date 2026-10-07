// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// Release discovery and AutoPatch against the test API server (the shared
// ConsoleSettings singleton) and a fake install repository.

type updatesEnv struct {
	r     *UpdatesReconciler
	src   *fakeReleases
	clock *offsetClock
}

func newUpdatesEnv(t *testing.T, updates *kwerftv1.UpdateSettings) *updatesEnv {
	t.Helper()
	requireEnvtest(t)
	e := &updatesEnv{src: testReleases(), clock: &offsetClock{}}
	e.r = &UpdatesReconciler{Client: k8s, Source: e.src, Version: "0.5.0", Now: e.clock.Now}
	useSettings(t, kwerftv1.ConsoleSettingsSpec{Updates: updates})
	t.Cleanup(func() { cleanUpgrades(t) })
	return e
}

func (e *updatesEnv) reconcile(t *testing.T) (*kwerftv1.UpdatesStatus, ctrl.Result) {
	t.Helper()
	var res ctrl.Result
	for range 10 {
		var err error
		res, err = e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Requeue {
			break
		}
	}
	var s kwerftv1.ConsoleSettings
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		t.Fatal(err)
	}
	return s.Status.Updates, res
}

func annotateSettings(t *testing.T, key, value string) {
	t.Helper()
	ctx := context.Background()
	eventually(t, func() error {
		var s kwerftv1.ConsoleSettings
		if err := k8s.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return err
		}
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations[key] = value
		return k8s.Update(ctx, &s)
	})
}

func autoUpgrades(t *testing.T) []kwerftv1.Upgrade {
	t.Helper()
	var list kwerftv1.UpgradeList
	if err := k8s.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	var out []kwerftv1.Upgrade
	for _, u := range list.Items {
		if u.Annotations[kwerftv1.AnnotationRequestedBy] == kwerftv1.RequestedByAutoUpdate {
			out = append(out, u)
		}
	}
	return out
}

func TestDiscoveryWritesAvailableUpdates(t *testing.T) {
	e := newUpdatesEnv(t, nil) // Notify by default
	st, res := e.reconcile(t)
	if st == nil || st.CheckedAt == nil || st.Error != "" || st.Current == nil || st.Current.Kwerft != "0.5.0" {
		t.Fatalf("updates = %+v", st)
	}
	var versions []string
	for _, a := range st.Available {
		versions = append(versions, fmt.Sprintf("%s %s %s %v", a.Component, a.Version, a.Kind, a.Allowed))
		if a.Component == kwerftv1.UpgradeKwerft && a.Notes != "Release notes." {
			t.Errorf("notes of %s: %q", a.Version, a.Notes)
		}
	}
	if got := strings.Join(versions, ", "); got != "Kwerft 0.6.0 Minor true, Kwerft 0.5.1 Patch true" {
		t.Errorf("available = %s", got)
	}
	if res.RequeueAfter < 5*time.Hour || res.RequeueAfter > DefaultUpdateInterval {
		t.Errorf("next check in %s", res.RequeueAfter)
	}
	if len(autoUpgrades(t)) != 0 {
		t.Error("Notify created an upgrade")
	}

	// Not again before the interval…
	calls := e.src.calls.Load()
	e.reconcile(t)
	if e.src.calls.Load() != calls {
		t.Error("checked again within the interval")
	}
	// …unless an owner asks (Check now)…
	annotateSettings(t, kwerftv1.AnnotationCheckUpdatesRequested, time.Now().Add(time.Second).UTC().Format(time.RFC3339))
	e.clock.Advance(2 * time.Second)
	if st2, _ := e.reconcile(t); e.src.calls.Load() == calls || !st2.CheckedAt.After(st.CheckedAt.Time) {
		t.Error("Check now did not check")
	}
	// …or the interval passed; a failed check keeps what was found.
	calls = e.src.calls.Load()
	e.src.err = fmt.Errorf("HTTP 503")
	e.clock.Advance(DefaultUpdateInterval)
	st, _ = e.reconcile(t)
	if e.src.calls.Load() == calls || st.Error != "HTTP 503" || len(st.Available) != 2 {
		t.Errorf("after the interval: %+v", st)
	}
}

func TestDiscoveryOffMakesNoRequests(t *testing.T) {
	e := newUpdatesEnv(t, &kwerftv1.UpdateSettings{Policy: kwerftv1.UpdatesOff})
	st, res := e.reconcile(t)
	if e.src.calls.Load() != 0 {
		t.Errorf("%d requests with policy Off", e.src.calls.Load())
	}
	if st != nil && len(st.Available) > 0 || res.RequeueAfter != 0 {
		t.Errorf("updates = %+v, %v", st, res)
	}
}

// window opens 30 minutes ago for two hours, every day.
func openWindow() *kwerftv1.MaintenanceWindow {
	start := time.Now().UTC().Add(-30 * time.Minute)
	return &kwerftv1.MaintenanceWindow{Start: start.Format("15:04"), Duration: &metav1.Duration{Duration: 2 * time.Hour}}
}

func TestAutoPatchInstallsPatchesInTheWindow(t *testing.T) {
	e := newUpdatesEnv(t, &kwerftv1.UpdateSettings{Policy: kwerftv1.UpdatesAutoPatch, Window: openWindow()})
	// Outside the window: nothing, and come back when it opens.
	e.clock.Advance(3 * time.Hour)
	if _, res := e.reconcile(t); len(autoUpgrades(t)) != 0 || res.RequeueAfter > 22*time.Hour {
		t.Fatalf("outside the window: %d upgrades, next %s", len(autoUpgrades(t)), res.RequeueAfter)
	}
	e.clock.Reset()
	e.reconcile(t)
	ups := autoUpgrades(t)
	if len(ups) != 1 || ups[0].Spec.Version != "0.5.1" || ups[0].Spec.Component != kwerftv1.UpgradeKwerft ||
		!strings.HasPrefix(ups[0].Name, "kwerft-0.5.1-") {
		t.Fatalf("auto upgrades = %+v", ups)
	}
	// One at a time, and one attempt per release and window.
	e.reconcile(t)
	setUpgradePhase(t, ups[0].Name, kwerftv1.UpgradeCancelled)
	e.reconcile(t)
	if n := len(autoUpgrades(t)); n != 1 {
		t.Errorf("%d auto upgrades, want 1", n)
	}
}

func TestAutoPatchPauseAndResume(t *testing.T) {
	e := newUpdatesEnv(t, &kwerftv1.UpdateSettings{Policy: kwerftv1.UpdatesAutoPatch, Window: openWindow()})
	e.reconcile(t)
	// The Upgrade reconciler pauses AutoPatch after an auto-update that
	// changed something and rolled back (a failed preflight would not).
	ups := autoUpgrades(t)
	if len(ups) != 1 {
		t.Fatalf("auto upgrades = %d", len(ups))
	}
	setUpgradePhase(t, ups[0].Name, kwerftv1.UpgradeRolledBack)
	ur := &UpgradeReconciler{Client: k8s, APIReader: k8s, Namespace: GatewayNamespace}
	eventually(t, func() error {
		if _, err := ur.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: ups[0].Name}}); err != nil {
			return err
		}
		st, _ := e.reconcile(t)
		if st.AutoPatchPausedBy != ups[0].Name {
			return fmt.Errorf("paused by %q", st.AutoPatchPausedBy)
		}
		return nil
	})
	// Paused: no new attempt, even in a new window.
	e.clock.Advance(24 * time.Hour)
	e.reconcile(t)
	if n := len(autoUpgrades(t)); n != 1 {
		t.Fatalf("%d auto upgrades while paused", n)
	}
	// An owner resumes it.
	annotateSettings(t, kwerftv1.AnnotationResumeAutoPatch, ups[0].Name)
	if st, _ := e.reconcile(t); st.AutoPatchPausedBy != "" {
		t.Errorf("still paused by %s", st.AutoPatchPausedBy)
	}
	if n := len(autoUpgrades(t)); n != 2 {
		t.Errorf("%d auto upgrades after resuming, want 2", n)
	}
}

func TestPickAutoPatch(t *testing.T) {
	cur := upgrades.MustVersion("0.5.0")
	available := []kwerftv1.AvailableUpdate{
		{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0", Kind: "Minor", Allowed: true},
		{Component: kwerftv1.UpgradeKwerft, Version: "0.5.1", Kind: "Patch", Allowed: true},
		{Component: kwerftv1.UpgradeKwerft, Version: "0.5.3", Kind: "Patch", Allowed: false},
		{Component: kwerftv1.UpgradeKwerft, Version: "0.5.2", Kind: "Patch", Allowed: true},
		{Component: kwerftv1.UpgradeKubernetes, Version: "v1.37.2+k3s1", Kind: "Patch", Allowed: true},
		{Component: kwerftv1.UpgradeKubernetes, Version: "v1.38.1+k3s1", Kind: "Minor", Allowed: true},
	}
	current := &kwerftv1.UpgradeVersions{Kwerft: "0.5.0", Kubernetes: "v1.37.1+k3s1"}
	if got := pickAutoPatch(available, cur, current, false); got == nil || got.Version != "0.5.2" {
		t.Errorf("kwerft patch = %+v", got)
	}
	if got := pickAutoPatch(available[4:], cur, current, false); got != nil {
		t.Errorf("k3s patch without kubernetesPatches: %+v", got)
	}
	if got := pickAutoPatch(available[4:], cur, current, true); got == nil || got.Version != "v1.37.2+k3s1" {
		t.Errorf("k3s patch = %+v", got)
	}
	if got := pickAutoPatch(available[:1], cur, current, true); got != nil {
		t.Errorf("picked a minor: %+v", got)
	}
}
