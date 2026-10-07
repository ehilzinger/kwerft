// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// Upgrades of agent clusters and "Upgrade all" (docs/phase6-upgrades.md ›
// As built (G1)) against two API servers: the console's cluster and the
// agent cluster "edge" behind the test Registry. Every Upgrade is created
// as the user in its cluster; no controller moves them here.

type upgradesMulti struct {
	*multiEnv
	preflight *fakePreflight
	api       *upgradesAPI
	admin     *session
}

func newUpgradesMulti(t *testing.T, console string) *upgradesMulti {
	t.Helper()
	settingsFixture(t)
	pf := &fakePreflight{remote: map[string]bool{"edge": true}}
	pf.set(kwerftv1.UpgradeCheck{Check: controllers.CheckTarget, OK: true, Message: "fine"})
	m := &upgradesMulti{preflight: pf}
	m.multiEnv = newMultiConsole(t, withSettings(), func(cfg *Config) {
		cfg.Upgrades = pf
		cfg.System, cfg.SystemReader = cluster.admin, cluster.admin
		cfg.upgradesHook = func(u *upgradesAPI) {
			u.poll, u.ping = 50*time.Millisecond, time.Second
			u.version = console
			m.api = u
		}
	})
	m.admin = m.signIn(t, "admin@example.com", store.RoleAdmin)
	t.Cleanup(func() {
		ctx := context.Background()
		_ = cluster.admin.DeleteAllOf(ctx, &kwerftv1.Upgrade{})
		_ = m.edge.admin.DeleteAllOf(ctx, &kwerftv1.Upgrade{})
	})
	return m
}

// agentCluster records an agent cluster as the Cluster reconciler would.
func agentClusterObject(t *testing.T, name string, phase kwerftv1.ClusterPhase, version string) {
	t.Helper()
	ctx := context.Background()
	cl := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}
	if err := cluster.admin.Create(ctx, cl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), cl) })
	cl.Status = kwerftv1.ClusterStatus{Phase: phase, AgentVersion: version, KubernetesVersion: "v1.37.1+k3s1"}
	if err := cluster.admin.Status().Update(ctx, cl); err != nil {
		t.Fatal(err)
	}
}

// kubeletIn adds a ready node running k3s version v to a test cluster.
func kubeletIn(t *testing.T, tc *testCluster, name, v string) {
	t.Helper()
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := tc.admin.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.admin.Delete(context.Background(), n) })
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
	n.Status.NodeInfo.KubeletVersion = v
	if err := tc.admin.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func (m *upgradesMulti) edgeUpgrade(t *testing.T, name string) *kwerftv1.Upgrade {
	t.Helper()
	var u kwerftv1.Upgrade
	if err := m.edge.admin.Get(context.Background(), client.ObjectKey{Name: name}, &u); err != nil {
		t.Fatal(err)
	}
	return &u
}

func clusterRow(t *testing.T, view updatesJSON, name string) clusterUpdatesJSON {
	t.Helper()
	i := slices.IndexFunc(view.Clusters, func(c clusterUpdatesJSON) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no row for %s: %+v", name, view.Clusters)
	}
	return view.Clusters[i]
}

func TestAgentClusterUpgrades(t *testing.T) {
	m := newUpgradesMulti(t, "0.6.0")
	agentClusterObject(t, "edge", controllers.ClusterConnected, "v0.5.0")
	kubeletIn(t, cluster, "upd-multi-local", "v1.37.1+k3s1")
	kubeletIn(t, m.edge, "upd-multi-edge", "v1.37.1+k3s1")

	// The versions table: the agent's real versions and its targets (the
	// console's release, and its own release's k3s pin).
	var view updatesJSON
	if code := m.owner.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK {
		t.Fatalf("overview: %d", code)
	}
	edge := clusterRow(t, view, "edge")
	if !edge.Connected || !edge.Upgradable || edge.Kwerft != "0.5.0" || edge.Kubernetes != "v1.37.1+k3s1" || len(edge.Available) != 2 ||
		edge.Available[0].Version != "0.6.0" || edge.Available[0].Kind != "Minor" || edge.Available[1].Version != "v1.37.2+k3s1" {
		t.Fatalf("edge row %+v", edge)
	}
	if !slices.Contains(m.preflight.targets, "0.5.0/v1.37.1+k3s1") {
		t.Errorf("Kubernetes targets asked for %v", m.preflight.targets)
	}
	if ua := view.UpgradeAll; ua == nil || ua.Version != "0.6.0" || ua.Console || !slices.Equal(ua.Clusters, []string{"edge"}) {
		t.Errorf("upgrade all %+v", view.UpgradeAll)
	}

	// The preflight runs for the agent's cluster, from the agent's version.
	var pf preflightJSON
	body := map[string]any{"cluster": "edge", "component": "Kwerft", "version": "0.6.0", "password": testPassword}
	if code := m.owner.do(t, "POST", "/api/v1/upgrades/preflight", body, &pf); code != http.StatusOK || pf.Cluster != "edge" || pf.From != "0.5.0" ||
		pf.Kind != "Minor" || m.preflight.in[len(m.preflight.in)-1] != "edge" {
		t.Errorf("preflight: %d %+v %v", code, pf, m.preflight.in)
	}
	var k3s preflightJSON
	if code := m.owner.do(t, "POST", "/api/v1/upgrades/preflight", map[string]any{"cluster": "edge", "component": "Kubernetes", "version": "v1.37.2+k3s1"}, &k3s); code != http.StatusOK ||
		k3s.From != "v1.37.1+k3s1" || k3s.Kind != "Patch" || k3s.ConfirmVersion {
		t.Errorf("k3s preflight: %d %+v", code, k3s)
	}

	// Started: created in the agent's cluster, as the owner.
	var started startAnswer
	if code := m.owner.do(t, "POST", "/api/v1/upgrades", body, &started); code != http.StatusCreated || started.Upgrade.Cluster != "edge" {
		t.Fatalf("start: %d %+v", code, started)
	}
	name := started.Upgrade.Name
	if u := m.edgeUpgrade(t, name); u.Spec.Version != "0.6.0" || u.Annotations[kwerftv1.AnnotationRequestedBy] != "owner@example.com" {
		t.Errorf("created %+v", u)
	}
	var local kwerftv1.UpgradeList
	if err := cluster.admin.List(context.Background(), &local); err != nil || len(local.Items) != 0 {
		t.Errorf("created in the console's cluster: %v %v", local.Items, err)
	}
	if a := upgradeAudit(t, m.store, "upgrade.start"); a == nil || a.Target != name || !strings.Contains(a.Detail, "Kwerft 0.5.0 → 0.6.0 on edge") {
		t.Errorf("audit %+v", a)
	}

	// History, detail, events and log through ?cluster=edge.
	var history []upgradeJSON
	if code := m.admin.do(t, "GET", "/api/v1/upgrades?cluster=edge", nil, &history); code != http.StatusOK || len(history) != 1 || history[0].Cluster != "edge" {
		t.Errorf("edge history: %d %+v", code, history)
	}
	if code := m.admin.do(t, "GET", "/api/v1/upgrades", nil, &history); code != http.StatusOK || !slices.ContainsFunc(history, func(u upgradeJSON) bool { return u.Name == name && u.Cluster == "edge" }) {
		t.Errorf("all history: %d %+v", code, history)
	}
	var one upgradeJSON
	if code := m.admin.do(t, "GET", "/api/v1/upgrades/"+name+"?cluster=edge", nil, &one); code != http.StatusOK || one.Cluster != "edge" || one.Phase != "Pending" {
		t.Errorf("detail: %d %+v", code, one)
	}
	if code := m.admin.do(t, "GET", "/api/v1/upgrades/"+name, nil, nil); code != http.StatusNotFound {
		t.Errorf("detail without the cluster: %d", code)
	}
	events, stop := upgradeEvents(t, m.admin, "/api/v1/upgrades/"+name+"/events?cluster=edge")
	defer stop()
	if ev := nextEvent(t, events); ev.name != "upgrade" || !strings.Contains(ev.data, `"cluster":"edge"`) {
		t.Fatalf("first event %+v", ev)
	}
	u := m.edgeUpgrade(t, name)
	u.Status.Phase = kwerftv1.UpgradeRunning
	if err := m.edge.admin.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events); ev.name != "upgrade" || !strings.Contains(ev.data, `"phase":"Running"`) {
		t.Fatalf("running event %+v", ev)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: upgrades.LogConfigMapName(name)},
		Data: map[string]string{upgrades.LogKey: "▸ Kwerft installer 0.6.0 (agent)\n"}}
	if err := m.edge.admin.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.edge.admin.Delete(context.Background(), cm) })
	var log struct {
		Log string `json:"log"`
	}
	if code := m.owner.do(t, "GET", "/api/v1/upgrades/"+name+"/log?cluster=edge", nil, &log); code != http.StatusOK || !strings.Contains(log.Log, "(agent)") {
		t.Errorf("log: %d %+v", code, log)
	}

	// Under way there: the row shows it, and nothing is left for "Upgrade all".
	view = updatesJSON{}
	if code := m.owner.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK {
		t.Fatal(code)
	}
	if edge := clusterRow(t, view, "edge"); edge.Active == nil || edge.Active.Name != name || view.UpgradeAll != nil {
		t.Errorf("edge row %+v, upgrade all %+v", edge, view.UpgradeAll)
	}

	// An agent the console cannot upgrade (from before console upgrades).
	m.preflight.mu.Lock()
	m.preflight.remote["edge"] = false
	m.preflight.mu.Unlock()
	if code := m.owner.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK || clusterRow(t, view, "edge").Upgradable {
		t.Errorf("unsupported edge: %d %+v", code, clusterRow(t, view, "edge"))
	}
	var refused apiError
	body["component"] = "Kubernetes"
	body["version"] = "v1.37.2+k3s1"
	if code := m.owner.do(t, "POST", "/api/v1/upgrades", body, &refused); code != http.StatusConflict || !strings.Contains(refused.Error, "Re-run the installer") {
		t.Errorf("unsupported start: %d %+v", code, refused)
	}
}

type upgradeAllAnswerJSON struct {
	upgradeAllAnswer
	Error string `json:"error"`
	Field string `json:"field"`
}

func TestUpgradeAll(t *testing.T) {
	m := newUpgradesMulti(t, "0.6.0")
	agentClusterObject(t, "edge", controllers.ClusterConnected, "0.5.0")
	agentClusterObject(t, "away", controllers.ClusterDisconnected, "0.5.0")
	kubeletIn(t, cluster, "upd-all-local", "v1.37.1+k3s1")

	// Refused before anything is planned: bad versions, older than the
	// console, the password, admins.
	for _, bad := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"version": "v0.6.0", "password": testPassword}, "version"},
		{map[string]any{"version": "latest", "password": testPassword}, "version"},
		{map[string]any{"version": "0.5.9", "password": testPassword}, "version"},
		{map[string]any{"version": "0.6.0"}, "password"},
		{map[string]any{"version": "0.6.0", "password": "wrong password"}, "password"},
	} {
		var a upgradeAllAnswerJSON
		if code := m.owner.do(t, "POST", "/api/v1/upgrades/all", bad.body, &a); code != http.StatusBadRequest || a.Field != bad.field {
			t.Errorf("%v: %d %+v, want field %s", bad.body, code, a, bad.field)
		}
	}
	if code := m.admin.do(t, "POST", "/api/v1/upgrades/all", map[string]any{"version": "0.6.0", "password": testPassword}, nil); code != http.StatusForbidden {
		t.Errorf("admin: %d", code)
	}

	// The console runs 0.6.0 already: only its agents follow, one fleet.
	var a upgradeAllAnswerJSON
	if code := m.owner.do(t, "POST", "/api/v1/upgrades/all", map[string]any{"version": "0.6.0", "password": testPassword}, &a); code != http.StatusCreated {
		t.Fatalf("upgrade all: %d %+v", code, a)
	}
	if a.Upgrade != nil || !strings.HasPrefix(a.Fleet, "fleet-") || len(a.Members) != 1 || a.Members[0].Cluster != "edge" || !a.Members[0].Held ||
		a.Members[0].Fleet != a.Fleet {
		t.Fatalf("answer %+v", a)
	}
	if !slices.ContainsFunc(a.Skipped, func(s fleetSkipJSON) bool {
		return s.Cluster == "away" && s.Warning && strings.Contains(s.Reason, "not connected")
	}) {
		t.Errorf("skipped %+v", a.Skipped)
	}
	member := m.edgeUpgrade(t, a.Members[0].Name)
	if member.Labels[kwerftv1.LabelFleet] != a.Fleet || member.Annotations[kwerftv1.AnnotationHold] == "" || member.Annotations[kwerftv1.AnnotationFleetOrder] != "0" ||
		member.Annotations[kwerftv1.AnnotationAfterUpgrade] != "" || member.Annotations[kwerftv1.AnnotationRequestedBy] != "owner@example.com" || member.Spec.Version != "0.6.0" {
		t.Errorf("member %+v", member.ObjectMeta)
	}
	if au := upgradeAudit(t, m.store, "upgrade.start_all"); au == nil || au.Target != a.Fleet || !strings.Contains(au.Detail, "Kwerft 0.6.0: edge") ||
		!strings.Contains(au.Detail, "skipped away") {
		t.Errorf("audit %+v", au)
	}
	// Again: edge is under way, so nothing is left to do.
	var again upgradeAllAnswerJSON
	if code := m.owner.do(t, "POST", "/api/v1/upgrades/all", map[string]any{"version": "0.6.0", "password": testPassword}, &again); code != http.StatusConflict ||
		!strings.Contains(again.Error, "Nothing to upgrade") {
		t.Errorf("again: %d %+v", code, again)
	}
	if err := m.edge.admin.Delete(context.Background(), member); err != nil {
		t.Fatal(err)
	}

	// The console on 0.5.0 with 0.6.0 on offer: the console's upgrade
	// first, the agents held after it.
	m.api.version = "0.5.0"
	setUpdatesStatus(t, &kwerftv1.UpdatesStatus{Available: []kwerftv1.AvailableUpdate{
		{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0", Kind: "Minor", Allowed: true},
		{Component: kwerftv1.UpgradeKwerft, Version: "0.7.0", Kind: "Minor", Allowed: false, Reason: "Upgrade to 0.6.0 first."},
	}})
	var view updatesJSON
	if code := m.owner.do(t, "GET", "/api/v1/updates", nil, &view); code != http.StatusOK {
		t.Fatal(code)
	}
	if ua := view.UpgradeAll; ua == nil || ua.Version != "0.6.0" || !ua.Console || !slices.Equal(ua.Clusters, []string{"edge"}) ||
		!slices.ContainsFunc(ua.Skipped, func(s fleetSkipJSON) bool { return s.Cluster == "away" }) {
		t.Errorf("upgrade all %+v", view.UpgradeAll)
	}
	var adminView updatesJSON
	if code := m.admin.do(t, "GET", "/api/v1/updates", nil, &adminView); code != http.StatusOK || adminView.UpgradeAll != nil {
		t.Errorf("admins get the button: %d %+v", code, adminView.UpgradeAll)
	}

	// A blocking check of the console's preflight creates nothing anywhere.
	m.preflight.set(kwerftv1.UpgradeCheck{Check: controllers.CheckAgentSkew, Message: "edge is too far behind"})
	var blocked upgradeAllAnswerJSON
	if code := m.owner.do(t, "POST", "/api/v1/upgrades/all", map[string]any{"version": "0.6.0", "password": testPassword}, &blocked); code != http.StatusConflict ||
		!strings.Contains(blocked.Error, "too far behind") {
		t.Errorf("blocked: %d %+v", code, blocked)
	}
	var edgeList kwerftv1.UpgradeList
	if err := m.edge.admin.List(context.Background(), &edgeList); err != nil || len(edgeList.Items) != 0 {
		t.Fatalf("members created after a blocked preflight: %v %v", edgeList.Items, err)
	}

	m.preflight.set(kwerftv1.UpgradeCheck{Check: controllers.CheckTarget, OK: true, Message: "0.5.0 → 0.6.0"})
	a = upgradeAllAnswerJSON{}
	if code := m.owner.do(t, "POST", "/api/v1/upgrades/all", map[string]any{"version": "0.6.0", "password": testPassword}, &a); code != http.StatusCreated {
		t.Fatalf("upgrade all with the console: %d %+v", code, a)
	}
	if a.Upgrade == nil || a.Upgrade.Cluster != "local" || a.Fleet != a.Upgrade.Name || a.Preflight == nil || len(a.Members) != 1 {
		t.Fatalf("answer %+v", a)
	}
	member = m.edgeUpgrade(t, a.Members[0].Name)
	if member.Annotations[kwerftv1.AnnotationAfterUpgrade] != a.Upgrade.Name || member.Labels[kwerftv1.LabelFleet] != a.Upgrade.Name ||
		!strings.Contains(member.Annotations[kwerftv1.AnnotationHold], a.Upgrade.Name) {
		t.Errorf("member %+v", member.ObjectMeta)
	}
	var consoleUp kwerftv1.Upgrade
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: a.Upgrade.Name}, &consoleUp); err != nil ||
		consoleUp.Annotations[kwerftv1.AnnotationRequestedBy] != "owner@example.com" {
		t.Fatalf("console upgrade %+v %v", consoleUp.ObjectMeta, err)
	}

	// The console's upgrade shows its agent clusters (the AgentUpgrades
	// reconciler's condition); a member shows that it is held.
	cond := metav1.Condition{Type: controllers.ConditionAgentClusters, Status: metav1.ConditionFalse, Reason: "Upgrading",
		Message: "Agent clusters (edge: waiting).", LastTransitionTime: metav1.Now()}
	setUpgradeStatus(t, consoleUp.Name, func(s *kwerftv1.UpgradeStatus) {
		s.Phase = kwerftv1.UpgradeSucceeded
		s.Conditions = []metav1.Condition{cond}
	})
	var one upgradeJSON
	if code := m.owner.do(t, "GET", "/api/v1/upgrades/"+consoleUp.Name, nil, &one); code != http.StatusOK || one.AgentClusters == nil ||
		one.AgentClusters.Finished || one.AgentClusters.Message != "Agent clusters (edge: waiting)." {
		t.Errorf("console upgrade view: %d %+v", code, one.AgentClusters)
	}
	raw, _ := json.Marshal(a.Members[0])
	if !strings.Contains(string(raw), `"held":true`) {
		t.Errorf("member view %s", raw)
	}
}
