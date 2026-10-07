// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"slices"
	"strings"
	"testing"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const apiServerMetrics = `# HELP apiserver_requested_deprecated_apis [STABLE] Gauge of deprecated APIs that have been requested, broken out by API group, version, resource, subresource, and removed_release.
# TYPE apiserver_requested_deprecated_apis gauge
apiserver_requested_deprecated_apis{group="flowcontrol.apiserver.k8s.io",removed_release="1.38",resource="flowschemas",subresource="",version="v1beta3"} 1
apiserver_requested_deprecated_apis{group="",removed_release="",resource="componentstatuses",subresource="",version="v1"} 1
apiserver_requested_deprecated_apis{group="batch",removed_release="1.25",resource="cronjobs",subresource="status",version="v1beta1"} 1
apiserver_requested_deprecated_apis{group="policy",removed_release="1.39",resource="poddisruptionbudgets",subresource="",version="v1beta1"} 1
apiserver_requested_deprecated_apis{group="old",removed_release="1.30",resource="things",subresource="",version="v1"} 0
apiserver_request_total{code="200",verb="GET"} 12
`

func TestParseDeprecatedAPIs(t *testing.T) {
	apis := ParseDeprecatedAPIs([]byte(apiServerMetrics))
	var names []string
	for _, a := range apis {
		names = append(names, a.String()+"@"+a.RemovedRelease)
	}
	want := []string{"flowcontrol.apiserver.k8s.io/v1beta3 flowschemas@1.38", "v1 componentstatuses@", "batch/v1beta1 cronjobs/status@1.25",
		"policy/v1beta1 poddisruptionbudgets@1.39"}
	if !slices.Equal(names, want) {
		t.Fatalf("apis = %v, want %v", names, want)
	}
	target := MustVersion("v1.38.1+k3s1")
	var removed []string
	for _, a := range apis {
		if a.RemovedBy(target) {
			removed = append(removed, a.Resource)
		}
	}
	if !slices.Equal(removed, []string{"flowschemas", "cronjobs"}) {
		t.Errorf("removed by 1.38 = %v", removed)
	}
	if got := parseLabels(`a="x\"y",b="",c="1\\2"`); got["a"] != `x"y` || got["b"] != "" || got["c"] != `1\2` {
		t.Errorf("labels = %q", got)
	}
}

func TestIsK3sVersion(t *testing.T) {
	for v, want := range map[string]bool{"v1.38.1+k3s1": true, "v1.38.1-rc1+k3s2": true, "0.6.0": false, "v1.38.1": false, "1.38.1+k3s1": false} {
		if got := IsK3sVersion(v); got != want {
			t.Errorf("IsK3sVersion(%q) = %v", v, got)
		}
	}
}

func TestRestoreHint(t *testing.T) {
	h := RestoreHint("pre-kubernetes-v1-38-1-k3s1-ab", "v1.37.2+k3s1")
	for _, want := range []string{"pre-kubernetes-v1-38-1-k3s1-ab", "--cluster-reset-restore-path=", "put k3s v1.37.2+k3s1 back", "k3s etcd-snapshot ls", K3sSnapshotDir} {
		if !strings.Contains(h, want) {
			t.Errorf("hint lacks %q: %s", want, h)
		}
	}
}

func TestReachedRunning(t *testing.T) {
	kw := func(st kwerftv1.UpgradeStatus) *kwerftv1.Upgrade {
		return &kwerftv1.Upgrade{Spec: kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft}, Status: st}
	}
	k8 := func(st kwerftv1.UpgradeStatus) *kwerftv1.Upgrade {
		return &kwerftv1.Upgrade{Spec: kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKubernetes}, Status: st}
	}
	cases := []struct {
		u    *kwerftv1.Upgrade
		want bool
	}{
		{kw(kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeFailed, Reason: "Preflight"}), false},
		{kw(kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeFailed, Reason: "Backup", Backup: &kwerftv1.UpgradeBackup{EtcdSnapshot: "pre-x"}}), false},
		{kw(kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeFailed, Reason: "Runner", Backup: &kwerftv1.UpgradeBackup{HelmRevisions: map[string]int32{"a/b": 1}}}), true},
		{kw(kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeFailed, Steps: []kwerftv1.UpgradeStep{{ID: "kwerft"}}}), true},
		{kw(kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeRolledBack}), true},
		{k8(kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeFailed, Reason: "Backup"}), false},
		{k8(kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeFailed, Nodes: []kwerftv1.UpgradeNode{{Name: "n"}}}), true},
	}
	for i, c := range cases {
		if got := ReachedRunning(c.u); got != c.want {
			t.Errorf("%d: ReachedRunning = %v, want %v", i, got, c.want)
		}
	}
}

func TestAgentTargetAllowed(t *testing.T) {
	cases := []struct {
		console, agent, target string
		ok                     bool
	}{
		{"0.6.0", "0.5.2", "0.6.0", true},
		{"0.6.1", "0.6.0", "0.6.1", true},
		{"0.6.0", "0.5.2", "0.6.1", false}, // past the console
		{"0.6.0", "0.6.0", "0.6.0", false}, // nothing to do
		{"0.6.0-dev", "0.5.0", "0.6.0", false},
	}
	for _, c := range cases {
		if err := AgentTargetAllowed(c.console, c.agent, c.target); (err == nil) != c.ok {
			t.Errorf("AgentTargetAllowed(%s, %s, %s) = %v", c.console, c.agent, c.target, err)
		}
	}
	if !AgentCompatible("0.6.0", "0.5.3") || !AgentCompatible("0.6.0", "0.6.0") || AgentCompatible("0.6.0", "0.4.9") || AgentCompatible("0.6.0", "0.7.0") {
		t.Error("AgentCompatible")
	}
}

func TestPlanFleet(t *testing.T) {
	queue, skipped := PlanFleet("0.6.0", []AgentCluster{
		{Name: "edge-2", Connected: true, AgentVersion: "0.5.1"},
		{Name: "edge-1", Connected: true, AgentVersion: "0.5.0"},
		{Name: "away", Connected: false, AgentVersion: "0.5.0"},
		{Name: "new", Connected: true, AgentVersion: "0.6.0"},
		{Name: "odd", Connected: true, AgentVersion: "dev"},
	})
	var names []string
	for _, a := range queue {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"edge-1", "edge-2"}) {
		t.Errorf("queue = %v", names)
	}
	var skips []string
	for _, s := range skipped {
		skips = append(skips, s.Cluster+":"+map[bool]string{true: "warn", false: "ok"}[s.Warning])
	}
	if !slices.Equal(skips, []string{"away:warn", "new:ok", "odd:warn"}) {
		t.Errorf("skipped = %v", skips)
	}
	if id := NewFleetID(); !strings.HasPrefix(id, "fleet-") || len(id) != 16 {
		t.Errorf("fleet id %q", id)
	}
}
