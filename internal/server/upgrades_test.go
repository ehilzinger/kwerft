package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// Settings › Updates through the console API against the test cluster with
// the chart's RBAC. No Upgrade controller runs here: the tests move the
// Upgrades' status themselves.

// fakePreflight answers the synchronous preflight with fixed checks.
type fakePreflight struct {
	mu     sync.Mutex
	checks []kwerftv1.UpgradeCheck
	calls  []kwerftv1.UpgradeSpec
}

func (f *fakePreflight) Supports(cluster string) bool { return cluster == clusters.Local }

func (f *fakePreflight) Preflight(_ context.Context, cluster string, spec kwerftv1.UpgradeSpec) ([]kwerftv1.UpgradeCheck, error) {
	if cluster != clusters.Local {
		return nil, ErrUpgradeUnsupported
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec)
	return slices.Clone(f.checks), nil
}

func (f *fakePreflight) set(checks ...kwerftv1.UpgradeCheck) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = checks
}

const testPassword = "a long test password"

type upgradesEnv struct {
	*console
	admin     *session
	preflight *fakePreflight
}

func newUpgradesEnv(t *testing.T) *upgradesEnv {
	t.Helper()
	requireCluster(t)
	settingsFixture(t)
	t.Cleanup(func() {
		_ = cluster.admin.DeleteAllOf(context.Background(), &kwerftv1.Upgrade{})
	})
	pf := &fakePreflight{}
	pf.set(kwerftv1.UpgradeCheck{Check: controllers.CheckTarget, OK: true, Message: "fine"})
	c := newConsole(t, withSettings(), func(cfg *Config) {
		cfg.Upgrades = pf
		cfg.System, cfg.SystemReader = cluster.admin, cluster.admin
		cfg.upgradesHook = func(u *upgradesAPI) { u.poll, u.ping = 50*time.Millisecond, time.Second }
	})
	return &upgradesEnv{console: c, admin: c.signIn(t, "admin@example.com", store.RoleAdmin), preflight: pf}
}

// kubelet adds a ready node running k3s version v.
func kubelet(t *testing.T, name, v string) {
	t.Helper()
	testNode(t, name, nil)
	ctx := context.Background()
	var n corev1.Node
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	n.Status.NodeInfo.KubeletVersion = v
	if err := cluster.admin.Status().Update(ctx, &n); err != nil {
		t.Fatal(err)
	}
}

func setUpgradeStatus(t *testing.T, name string, mutate func(*kwerftv1.UpgradeStatus)) {
	t.Helper()
	ctx := context.Background()
	var u kwerftv1.Upgrade
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: name}, &u); err != nil {
		t.Fatal(err)
	}
	mutate(&u.Status)
	if err := cluster.admin.Status().Update(ctx, &u); err != nil {
		t.Fatal(err)
	}
}

func setUpdatesStatus(t *testing.T, st *kwerftv1.UpdatesStatus) {
	t.Helper()
	cs := settingsNow(t)
	cs.Status.Updates = st
	if err := cluster.admin.Status().Update(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
}

func upgradeAudit(t *testing.T, st *store.Store, action string) *store.AuditEntry {
	t.Helper()
	entries, err := st.RecentAudit(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if entries[i].Action == action {
			return &entries[i]
		}
	}
	return nil
}

type startAnswer struct {
	Upgrade   upgradeJSON   `json:"upgrade"`
	Preflight preflightJSON `json:"preflight"`
	Error     string        `json:"error"`
	Field     string        `json:"field"`
}

func TestUpdatesRoles(t *testing.T) {
	e := newUpgradesEnv(t)
	start := map[string]any{"component": "Kwerft", "version": "9.9.0", "password": testPassword}
	for _, s := range []*session{e.dev, e.viewer} {
		for _, req := range []struct{ method, path string }{
			{"GET", "/api/v1/updates"}, {"GET", "/api/v1/upgrades"}, {"POST", "/api/v1/updates/check"},
			{"POST", "/api/v1/upgrades"}, {"PUT", "/api/v1/settings/updates"}, {"GET", "/api/v1/upgrades/x"},
		} {
			if code := s.do(t, req.method, req.path, start, nil); code != http.StatusForbidden {
				t.Errorf("developer/viewer %s %s: %d", req.method, req.path, code)
			}
		}
	}
	// Admins read and ask for a check, but start, cancel and set nothing.
	var view updatesJSON
	if code := e.admin.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK || view.CanUpgrade {
		t.Errorf("admin reads: %d %+v", code, view)
	}
	if code := e.admin.do(t, "GET", "/api/v1/upgrades", nil, nil); code != http.StatusOK {
		t.Errorf("admin history: %d", code)
	}
	if code := e.admin.do(t, "POST", "/api/v1/updates/check", nil, nil); code != http.StatusAccepted {
		t.Errorf("admin check: %d", code)
	}
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/upgrades", start},
		{"POST", "/api/v1/upgrades/preflight", start},
		{"PUT", "/api/v1/settings/updates", map[string]any{"policy": "Off"}},
		{"POST", "/api/v1/updates/resume-autopatch", nil},
		{"DELETE", "/api/v1/upgrades/x", nil},
	} {
		if code := e.admin.do(t, req.method, req.path, req.body, nil); code != http.StatusForbidden {
			t.Errorf("admin %s %s: %d", req.method, req.path, code)
		}
	}
	// Kubernetes agrees: an admin cannot create or patch an Upgrade even
	// past the console's role check.
	for _, verb := range []string{"create", "patch", "delete"} {
		if can(t, store.RoleAdmin, "", verb, "kwerft.dev", "upgrades", "") {
			t.Errorf("RBAC lets admins %s upgrades", verb)
		}
		if !can(t, store.RoleOwner, "", verb, "kwerft.dev", "upgrades", "") {
			t.Errorf("RBAC does not let owners %s upgrades", verb)
		}
	}
	if !can(t, store.RoleAdmin, "", "list", "kwerft.dev", "upgrades", "") {
		t.Error("RBAC does not let admins list upgrades")
	}
	if len(e.preflight.calls) != 0 {
		t.Errorf("preflight ran for a refused request: %v", e.preflight.calls)
	}
}

func TestUpdatesOverview(t *testing.T) {
	e := newUpgradesEnv(t)
	kubelet(t, "upd-node-1", "v1.37.1+k3s1")
	checked := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	notes := "Faster everything.\n\n## Install\n\nOn a fresh Ubuntu server:\n\n```bash\ncurl … | sudo bash\n```\n\n| Image | x |\n\n## What's Changed\n\n* Fix by @someone"
	setUpdatesStatus(t, &kwerftv1.UpdatesStatus{
		CheckedAt: &checked, Current: &kwerftv1.UpgradeVersions{Kwerft: "0.5.0", Kubernetes: "v1.37.1+k3s1"},
		Available: []kwerftv1.AvailableUpdate{
			{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0", Kind: "Minor", Notes: notes, Allowed: true},
			{Component: kwerftv1.UpgradeKubernetes, Version: "v1.37.2+k3s1", Kind: "Patch", Allowed: true},
		},
		AutoPatchPausedBy: "kwerft-0.5.1-abcde",
	})
	var view updatesJSON
	if code := e.owner.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK {
		t.Fatalf("read: %d", code)
	}
	if !view.CanUpgrade || view.Policy.Policy != "Notify" || view.Policy.Channel != "stable" || view.CheckedAt == nil || view.Checking ||
		view.AutoPatchPausedBy != "kwerft-0.5.1-abcde" || view.Current.Kubernetes != "v1.37.1+k3s1" || view.Current.Kwerft == "" {
		t.Errorf("view %+v", view)
	}
	if len(view.Available) != 2 || view.Available[0].Notes != "Faster everything.\n\n## What's Changed\n\n* Fix by @someone" {
		t.Errorf("available %+v", view.Available)
	}
	if len(view.Clusters) != 1 || view.Clusters[0].Name != "local" || !view.Clusters[0].Upgradable || view.Clusters[0].Kubernetes != "v1.37.1+k3s1" ||
		len(view.Clusters[0].Available) != 2 || view.Clusters[0].Active != nil {
		t.Errorf("clusters %+v", view.Clusters)
	}

	// Check now: an annotation the Updates reconciler acts on.
	if code := e.owner.do(t, "POST", "/api/v1/updates/check", nil, nil); code != http.StatusAccepted {
		t.Fatalf("check: %d", code)
	}
	if at, err := time.Parse(time.RFC3339, settingsNow(t).Annotations[kwerftv1.AnnotationCheckUpdatesRequested]); err != nil || at.Before(checked.Time) {
		t.Errorf("check annotation %v %v", at, err)
	}
	if code := e.owner.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK || !view.Checking {
		t.Errorf("after check: %d checking=%v", code, view.Checking)
	}

	// Resume AutoPatch names the auto-update that paused it.
	if code := e.owner.do(t, "POST", "/api/v1/updates/resume-autopatch", nil, nil); code != http.StatusAccepted {
		t.Fatalf("resume: %d", code)
	}
	if got := settingsNow(t).Annotations[kwerftv1.AnnotationResumeAutoPatch]; got != "kwerft-0.5.1-abcde" {
		t.Errorf("resume annotation %q", got)
	}
	if a := upgradeAudit(t, e.store, "updates.autopatch_resumed"); a == nil || a.Actor != "owner@example.com" {
		t.Errorf("audit %+v", a)
	}
	setUpdatesStatus(t, &kwerftv1.UpdatesStatus{CheckedAt: &checked})
	if code := e.owner.do(t, "POST", "/api/v1/updates/resume-autopatch", nil, nil); code != http.StatusConflict {
		t.Errorf("resume when not paused: %d", code)
	}

	// Off: nothing on offer, and no check.
	if code := e.owner.do(t, "PUT", "/api/v1/settings/updates", map[string]any{"policy": "Off"}, nil); code != http.StatusOK {
		t.Fatalf("off: %d", code)
	}
	if code := e.owner.do(t, "POST", "/api/v1/updates/check", nil, nil); code != http.StatusConflict {
		t.Errorf("check while off: %d", code)
	}
}

func TestUpdatePolicy(t *testing.T) {
	e := newUpgradesEnv(t)
	for _, bad := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"policy": "Always"}, "policy"},
		{map[string]any{"policy": "Notify", "channel": "nightly"}, "channel"},
		{map[string]any{"policy": "AutoPatch"}, "window"},
		{map[string]any{"policy": "AutoPatch", "window": map[string]any{"days": []string{"Sunday", "Funday"}, "start": "03:00"}}, "window.days"},
		{map[string]any{"policy": "AutoPatch", "window": map[string]any{"start": "3am"}}, "window.start"},
		{map[string]any{"policy": "AutoPatch", "window": map[string]any{"start": "03:00", "duration": "10m"}}, "window.duration"},
		{map[string]any{"policy": "AutoPatch", "window": map[string]any{"start": "03:00", "duration": "25h"}}, "window.duration"},
		{map[string]any{"policy": "AutoPatch", "window": map[string]any{"start": "03:00", "timeZone": "Mars/Olympus"}}, "window.timeZone"},
	} {
		var e2 apiError
		if code := e.owner.do(t, "PUT", "/api/v1/settings/updates", bad.body, &e2); code != http.StatusBadRequest || e2.Field != bad.field {
			t.Errorf("%v: %d %+v, want field %s", bad.body, code, e2, bad.field)
		}
	}
	var got updatePolicyJSON
	body := map[string]any{"policy": "AutoPatch", "channel": "edge", "kubernetesPatches": true,
		"window": map[string]any{"days": []string{"sun", "Sat", "sun"}, "start": "03:00", "duration": "90m", "timeZone": "Europe/Berlin"}}
	if code := e.owner.do(t, "PUT", "/api/v1/settings/updates", body, &got); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	if got.Policy != "AutoPatch" || got.Window == nil || !slices.Equal(got.Window.Days, []string{"Sat", "Sun"}) || got.Window.Duration != "1h30m" {
		t.Errorf("answer %+v %+v", got, got.Window)
	}
	u := settingsNow(t).Spec.Updates
	if u == nil || u.Policy != kwerftv1.UpdatesAutoPatch || u.Channel != "edge" || !u.KubernetesPatches || u.Window == nil ||
		u.Window.Start != "03:00" || u.Window.Duration.Duration != 90*time.Minute || u.Window.TimeZone != "Europe/Berlin" {
		t.Errorf("spec %+v", u)
	}
	a := upgradeAudit(t, e.store, "updates.policy")
	if a == nil || a.Actor != "owner@example.com" || !strings.Contains(a.Detail, "Notify, channel stable → AutoPatch, channel edge, Kubernetes patches, window Sat,Sun 03:00 for 1h30m Europe/Berlin") {
		t.Errorf("audit %+v", a)
	}
	var view updatesJSON
	if code := e.admin.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK || view.Policy.Window == nil || view.NextWindow == nil {
		t.Errorf("read back: %d %+v", code, view.Policy)
	}
	// Every day is no days; the window may stay while Notify.
	body = map[string]any{"policy": "Notify", "window": map[string]any{"days": []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, "start": "23:30"}}
	if code := e.owner.do(t, "PUT", "/api/v1/settings/updates", body, &got); code != http.StatusOK || len(got.Window.Days) != 0 || got.Window.Duration != "2h" {
		t.Errorf("every day: %d %+v", code, got.Window)
	}
	// Each save replaces the whole policy: nothing of the one before stays.
	u = settingsNow(t).Spec.Updates
	if u.Policy != kwerftv1.UpdatesNotify || u.Channel != "stable" || u.KubernetesPatches || u.Window == nil || len(u.Window.Days) != 0 ||
		u.Window.Start != "23:30" || u.Window.TimeZone != "" || u.Window.Duration.Duration != 2*time.Hour {
		t.Errorf("replaced spec %+v %+v", u, u.Window)
	}
	if code := e.owner.do(t, "PUT", "/api/v1/settings/updates", map[string]any{"policy": "Notify"}, nil); code != http.StatusOK {
		t.Fatalf("no window: %d", code)
	}
	if u = settingsNow(t).Spec.Updates; u.Window != nil {
		t.Errorf("the window stayed: %+v", u.Window)
	}
}

func TestStartUpgrade(t *testing.T) {
	e := newUpgradesEnv(t)
	kubelet(t, "upd-node-2", "v1.37.1+k3s1")
	kwerft := map[string]any{"component": "Kwerft", "version": "9.9.0", "password": testPassword}

	// Bad requests never reach the preflight.
	for _, bad := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"component": "Helm", "version": "1.0.0", "password": testPassword}, "component"},
		{map[string]any{"component": "Kwerft", "version": "latest", "password": testPassword}, "version"},
		{map[string]any{"component": "Kubernetes", "version": "1.38.1", "password": testPassword}, "version"},
		{map[string]any{"component": "Kwerft", "version": "9.9.0"}, "password"},
		{map[string]any{"component": "Kwerft", "version": "9.9.0", "password": "wrong password"}, "password"},
		// A Kubernetes minor: the version typed again.
		{map[string]any{"component": "Kubernetes", "version": "v1.38.1+k3s1", "password": testPassword}, "confirmVersion"},
		{map[string]any{"component": "Kubernetes", "version": "v1.38.1+k3s1", "password": testPassword, "confirmVersion": "v1.38.1"}, "confirmVersion"},
	} {
		var a startAnswer
		if code := e.owner.do(t, "POST", "/api/v1/upgrades", bad.body, &a); code != http.StatusBadRequest || a.Field != bad.field {
			t.Errorf("%v: %d %+v, want field %s", bad.body, code, a, bad.field)
		}
	}
	if len(e.preflight.calls) != 0 {
		t.Fatalf("preflight ran: %v", e.preflight.calls)
	}
	if a := upgradeAudit(t, e.store, "account.confirm_failed"); a == nil {
		t.Error("the wrong password was not audited")
	}
	var unknown apiError
	if code := e.owner.do(t, "POST", "/api/v1/upgrades", map[string]any{"cluster": "nowhere", "component": "Kwerft", "version": "9.9.0", "password": testPassword}, &unknown); code != http.StatusNotFound {
		t.Errorf("unknown cluster: %d %+v", code, unknown)
	}

	// The dialog's preflight: kind, confirmation, warnings.
	e.preflight.set(
		kwerftv1.UpgradeCheck{Check: controllers.CheckTarget, OK: true, Message: "0.5.0 → 9.9.0"},
		kwerftv1.UpgradeCheck{Check: controllers.CheckAgentSkew, Warning: true, Message: "skipped: far-away"},
		kwerftv1.UpgradeCheck{Check: controllers.CheckDataRollback, OK: false, Message: "9.9.0 is not rollback-safe"},
	)
	var pf preflightJSON
	if code := e.owner.do(t, "POST", "/api/v1/upgrades/preflight", kwerft, &pf); code != http.StatusOK || !pf.Blocked || !pf.DataRollback || pf.ConfirmVersion || len(pf.Checks) != 3 {
		t.Errorf("preflight: %d %+v", code, pf)
	}
	var minor preflightJSON
	if code := e.owner.do(t, "POST", "/api/v1/upgrades/preflight", map[string]any{"component": "Kubernetes", "version": "v1.38.1+k3s1"}, &minor); code != http.StatusOK ||
		minor.Kind != "Minor" || !minor.ConfirmVersion || minor.From != "v1.37.1+k3s1" {
		t.Errorf("k3s minor preflight: %d %+v", code, minor)
	}
	var patch preflightJSON
	if code := e.owner.do(t, "POST", "/api/v1/upgrades/preflight", map[string]any{"component": "Kubernetes", "version": "v1.37.2+k3s1"}, &patch); code != http.StatusOK ||
		patch.Kind != "Patch" || patch.ConfirmVersion {
		t.Errorf("k3s patch preflight: %d %+v", code, patch)
	}

	// A blocking check refuses the upgrade and creates nothing.
	var refused startAnswer
	if code := e.owner.do(t, "POST", "/api/v1/upgrades", kwerft, &refused); code != http.StatusConflict || !refused.Preflight.Blocked ||
		!strings.Contains(refused.Error, "not rollback-safe") {
		t.Errorf("blocked: %d %+v", code, refused)
	}
	var list kwerftv1.UpgradeList
	if err := cluster.admin.List(context.Background(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("an Upgrade was created: %v %v", list.Items, err)
	}

	// Accepted: created as the owner, audited.
	e.preflight.set(
		kwerftv1.UpgradeCheck{Check: controllers.CheckTarget, OK: true, Message: "0.5.0 → 9.9.0"},
		kwerftv1.UpgradeCheck{Check: controllers.CheckAgentSkew, Warning: true, Message: "skipped: far-away"},
		kwerftv1.UpgradeCheck{Check: controllers.CheckDataRollback, OK: true, Message: "Accepted: a rollback restores the database copy."},
	)
	kwerft["acceptDataRollback"] = true
	var ok startAnswer
	if code := e.owner.do(t, "POST", "/api/v1/upgrades", kwerft, &ok); code != http.StatusCreated {
		t.Fatalf("start: %d %+v", code, ok)
	}
	if !strings.HasPrefix(ok.Upgrade.Name, "kwerft-9.9.0-") || ok.Upgrade.Phase != "Pending" || !ok.Upgrade.Cancellable || ok.Upgrade.RequestedBy != "owner@example.com" ||
		ok.Preflight.Blocked || !ok.Preflight.DataRollback {
		t.Errorf("answer %+v", ok)
	}
	var created kwerftv1.Upgrade
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: ok.Upgrade.Name}, &created); err != nil {
		t.Fatal(err)
	}
	if created.Spec.Component != kwerftv1.UpgradeKwerft || created.Spec.Version != "9.9.0" || !created.Spec.AcceptDataRollback ||
		created.Annotations[kwerftv1.AnnotationRequestedBy] != "owner@example.com" {
		t.Errorf("created %+v", created)
	}
	if mf := created.ManagedFields; len(mf) == 0 || mf[0].Manager == "" {
		t.Logf("managed fields %v", mf)
	}
	if a := upgradeAudit(t, e.store, "upgrade.start"); a == nil || a.Target != created.Name || !strings.Contains(a.Detail, "→ 9.9.0 on local") ||
		!strings.Contains(a.Detail, "data rollback accepted") {
		t.Errorf("audit %+v", a)
	}
	if last := e.preflight.calls[len(e.preflight.calls)-1]; last.Version != "9.9.0" || !last.AcceptDataRollback {
		t.Errorf("preflight spec %+v", last)
	}

	// One unfinished Upgrade per component.
	var dup startAnswer
	if code := e.owner.do(t, "POST", "/api/v1/upgrades", kwerft, &dup); code != http.StatusConflict || !strings.Contains(dup.Error, created.Name) {
		t.Errorf("duplicate: %d %+v", code, dup)
	}

	// The typed confirmation lets a Kubernetes minor through.
	e.preflight.set(kwerftv1.UpgradeCheck{Check: controllers.CheckTarget, OK: true})
	var k3s startAnswer
	if code := e.owner.do(t, "POST", "/api/v1/upgrades", map[string]any{"component": "Kubernetes", "version": "v1.38.1+k3s1", "password": testPassword,
		"confirmVersion": "v1.38.1+k3s1"}, &k3s); code != http.StatusCreated || !strings.HasPrefix(k3s.Upgrade.Name, "kubernetes-v1.38.1-k3s1-") {
		t.Errorf("k3s minor: %d %+v", code, k3s)
	}

	// The history lists both, newest first, and the overview names the
	// active one.
	var history []upgradeJSON
	if code := e.admin.do(t, "GET", "/api/v1/upgrades", nil, &history); code != http.StatusOK || len(history) != 2 || history[0].Cluster != "local" {
		t.Fatalf("history: %d %+v", code, history)
	}
	var view updatesJSON
	if code := e.owner.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK || view.Clusters[0].Active == nil {
		t.Errorf("overview active: %d %+v", code, view.Clusters)
	}
	var one upgradeJSON
	if code := e.admin.do(t, "GET", "/api/v1/upgrades/"+created.Name+"?cluster=local", nil, &one); code != http.StatusOK || one.Name != created.Name {
		t.Errorf("detail: %d %+v", code, one)
	}
	if code := e.admin.do(t, "GET", "/api/v1/upgrades/no-such-upgrade", nil, nil); code != http.StatusNotFound {
		t.Errorf("missing: %d", code)
	}
}

func TestCancelUpgrade(t *testing.T) {
	e := newUpgradesEnv(t)
	var ok startAnswer
	if code := e.owner.do(t, "POST", "/api/v1/upgrades", map[string]any{"component": "Kwerft", "version": "9.9.0", "password": testPassword}, &ok); code != http.StatusCreated {
		t.Fatalf("start: %d %+v", code, ok)
	}
	name := ok.Upgrade.Name
	setUpgradeStatus(t, name, func(s *kwerftv1.UpgradeStatus) { s.Phase = kwerftv1.UpgradeBackingUp })
	var cancelled upgradeJSON
	if code := e.owner.do(t, "DELETE", "/api/v1/upgrades/"+name, nil, &cancelled); code != http.StatusAccepted {
		t.Fatalf("cancel: %d", code)
	}
	var u kwerftv1.Upgrade
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: name}, &u); err != nil || u.Annotations[kwerftv1.AnnotationCancelRequested] != "owner@example.com" {
		t.Errorf("annotation %v %v", u.Annotations, err)
	}
	if a := upgradeAudit(t, e.store, "upgrade.cancel"); a == nil || a.Target != name || !strings.Contains(a.Detail, "backup") {
		t.Errorf("audit %+v", a)
	}
	// Once the installer runs, it is too late.
	setUpgradeStatus(t, name, func(s *kwerftv1.UpgradeStatus) { s.Phase = kwerftv1.UpgradeRunning })
	var late apiError
	if code := e.owner.do(t, "DELETE", "/api/v1/upgrades/"+name, nil, &late); code != http.StatusConflict || !strings.Contains(late.Error, "rolls back by itself") {
		t.Errorf("cancel while running: %d %+v", code, late)
	}
	setUpgradeStatus(t, name, func(s *kwerftv1.UpgradeStatus) { s.Phase = kwerftv1.UpgradeSucceeded })
	if code := e.owner.do(t, "DELETE", "/api/v1/upgrades/"+name, nil, &late); code != http.StatusConflict {
		t.Errorf("cancel when done: %d", code)
	}
}

// sseEvent is one Server-Sent Event.
type sseEvent struct {
	name string
	data string
}

// readEvents streams GET path into a channel until the body ends.
func upgradeEvents(t *testing.T, s *session, path string) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", s.url+path, nil)
	res, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	out := make(chan sseEvent, 32)
	go func() {
		defer close(out)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				out <- ev
				ev = sseEvent{}
			}
		}
	}()
	return out, cancel
}

func nextEvent(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	return sseEvent{}
}

func TestUpgradeEventsAndLog(t *testing.T) {
	e := newUpgradesEnv(t)
	var ok startAnswer
	if code := e.owner.do(t, "POST", "/api/v1/upgrades", map[string]any{"component": "Kwerft", "version": "9.9.0", "password": testPassword}, &ok); code != http.StatusCreated {
		t.Fatalf("start: %d %+v", code, ok)
	}
	name := ok.Upgrade.Name
	if code := e.dev.do(t, "GET", "/api/v1/upgrades/"+name+"/events", nil, nil); code != http.StatusForbidden {
		t.Errorf("developer stream: %d", code)
	}
	events, stop := upgradeEvents(t, e.admin, "/api/v1/upgrades/"+name+"/events")
	defer stop()
	decode := func(ev sseEvent) upgradeJSON {
		var u upgradeJSON
		if err := json.Unmarshal([]byte(ev.data), &u); err != nil {
			t.Fatalf("%s: %v", ev.data, err)
		}
		return u
	}
	if ev := nextEvent(t, events); ev.name != "upgrade" || decode(ev).Phase != "Pending" {
		t.Fatalf("first event %+v", ev)
	}
	at := metav1.NewTime(time.Now().Truncate(time.Second))
	setUpgradeStatus(t, name, func(s *kwerftv1.UpgradeStatus) {
		s.Phase = kwerftv1.UpgradeRunning
		s.Steps = []kwerftv1.UpgradeStep{{ID: "preflight", Label: "Preflight", State: "Done", Detail: "Ubuntu 24.04", At: &at},
			{ID: "network", Label: "Network", State: "Running"}}
	})
	ev := nextEvent(t, events)
	if u := decode(ev); ev.name != "upgrade" || u.Phase != "Running" || len(u.Steps) != 2 || u.Steps[0].Detail != "Ubuntu 24.04" || u.Cancellable {
		t.Fatalf("running event %+v", u)
	}
	setUpgradeStatus(t, name, func(s *kwerftv1.UpgradeStatus) {
		s.Phase = kwerftv1.UpgradeRolledBack
		s.Reason, s.Message = "Kwerft", "The Kwerft stage failed; rolled back to 0.5.0."
		s.FinishedAt = &at
	})
	if u := decode(nextEvent(t, events)); u.Phase != "RolledBack" || !u.Finished || u.Reason != "Kwerft" {
		t.Fatalf("finished event %+v", u)
	}
	if ev := nextEvent(t, events); ev.name != "end" || !strings.Contains(ev.data, "RolledBack") {
		t.Fatalf("end event %+v", ev)
	}
	// A finished Upgrade's stream ends right after its state.
	again, stop2 := upgradeEvents(t, e.owner, "/api/v1/upgrades/"+name+"/events")
	defer stop2()
	if ev := nextEvent(t, again); ev.name != "upgrade" {
		t.Errorf("first %+v", ev)
	}
	if ev := nextEvent(t, again); ev.name != "end" {
		t.Errorf("then %+v", ev)
	}

	// The installer log, read with the console's identity.
	var log struct {
		Log       string `json:"log"`
		Truncated bool   `json:"truncated"`
	}
	if code := e.admin.do(t, "GET", "/api/v1/upgrades/"+name+"/log", nil, &log); code != http.StatusOK || log.Log != "" {
		t.Errorf("no log yet: %d %+v", code, log)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: upgrades.LogConfigMapName(name)},
		Data: map[string]string{upgrades.LogKey: "▸ Kwerft installer 9.9.0\n✓ Preflight\n"}}
	if err := cluster.admin.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), cm) })
	if code := e.owner.do(t, "GET", "/api/v1/upgrades/"+name+"/log", nil, &log); code != http.StatusOK || !strings.Contains(log.Log, "✓ Preflight") || log.Truncated {
		t.Errorf("log: %d %+v", code, log)
	}
	if code := e.viewer.do(t, "GET", "/api/v1/upgrades/"+name+"/log", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer log: %d", code)
	}
}

func TestReleaseNotes(t *testing.T) {
	for in, want := range map[string]string{
		"": "",
		"## Install\n\ncurl | bash\n\n| Image | x |\n":                                                             "",
		"Hand-written.\n\n## Install\n\nOn a fresh server\n\n**Not rollback-safe.** …\n\n## What's Changed\n* a\n": "Hand-written.\n\n## What's Changed\n* a",
		"## What's Changed\n* a\n## Install\nx\n":                                                                  "## What's Changed\n* a",
		"### Install notes stay\n## Installation\nalso stays":                                                      "### Install notes stay\n## Installation\nalso stays",
	} {
		if got := ReleaseNotes(in); got != want {
			t.Errorf("ReleaseNotes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeWindow(t *testing.T) {
	w, field, _ := normalizeWindow(&windowJSON{Days: []string{"sun", "MON"}, Start: "22:00", Duration: "4h", TimeZone: "Europe/Berlin"})
	if w == nil || field != "" || !slices.Equal(w.Days, []string{"Mon", "Sun"}) || w.Duration.Duration != 4*time.Hour {
		t.Fatalf("%+v %s", w, field)
	}
	for _, bad := range []windowJSON{
		{Start: "24:00"}, {Start: "03:00", Duration: "1h30s"}, {Start: "03:00", Days: []string{"Someday"}}, {Start: "03:00", TimeZone: "Nowhere/Land"},
	} {
		if w, field, _ := normalizeWindow(&bad); w != nil || field == "" {
			t.Errorf("%+v accepted", bad)
		}
	}
	if got := formatDuration(90 * time.Minute); got != "1h30m" {
		t.Errorf("formatDuration = %q", got)
	}
	if got := formatDuration(2 * time.Hour); got != "2h" {
		t.Errorf("formatDuration = %q", got)
	}
	if got := formatDuration(45 * time.Minute); got != "45m" {
		t.Errorf("formatDuration = %q", got)
	}
}
