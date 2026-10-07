// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The console API's preflight for agent clusters, against fake APIs: the
// management cluster (Cluster objects, NodePools) and one per agent.

func agentCluster(name string, phase kwerftv1.ClusterPhase, version string) *kwerftv1.Cluster {
	return &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: kwerftv1.ClusterStatus{Phase: phase, AgentVersion: version}}
}

func newConsolePreflight(t *testing.T, mgmtObjs []client.Object, edgeObjs ...client.Object) (*ConsolePreflight, *fakeReleases, *[]string) {
	t.Helper()
	local := testChecks(t)
	local.Version = "0.6.0"
	releases := local.Releases.(*fakeReleases)
	base := []client.Object{
		agentCluster("edge-1", ClusterConnected, "v0.5.0"),
		agentCluster("away", ClusterDisconnected, "0.5.0"),
		agentCluster("old", ClusterConnected, "0.4.0"),
	}
	mgmt := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(append(base, mgmtObjs...)...).Build()
	edgeBase := []client.Object{testNode("edge-1-server", true, map[string]string{kwerftv1.LabelInstaller: "true"})}
	edge := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(append(edgeBase, edgeObjs...)...).Build()
	// An agent from before console upgrades: its API knows no Upgrade kind.
	old := fake.NewClientBuilder().WithScheme(NewScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*kwerftv1.UpgradeList); ok {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "kwerft.dev", Kind: "Upgrade"}}
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	var asked []string
	p := &ConsolePreflight{
		Local: local, Management: mgmt, Clusters: fakeClusterClients{"edge-1": edge, "old": old}, Version: "0.6.0",
		APIServer: func(cluster string) (upgrades.APIServerInfo, error) {
			asked = append(asked, cluster)
			return &fakeAPIServer{}, nil
		},
	}
	return p, releases, &asked
}

func TestConsolePreflightSupports(t *testing.T) {
	p, _, _ := newConsolePreflight(t, nil)
	for cluster, want := range map[string]bool{clusters.Local: true, "edge-1": true, "away": false, "old": false, "nowhere": false} {
		if got := p.Supports(cluster); got != want {
			t.Errorf("Supports(%s) = %v, want %v", cluster, got, want)
		}
	}
	for _, cluster := range []string{"away", "old", "nowhere"} {
		if _, err := p.Preflight(context.Background(), cluster, kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"}); !errors.Is(err, ErrUpgradeUnsupported) {
			t.Errorf("Preflight(%s): %v", cluster, err)
		}
	}
	// No agent clusters at all (no registry): the local one only.
	if (&ConsolePreflight{Local: p.Local}).Supports("edge-1") {
		t.Error("an agent is supported without a way to reach it")
	}
}

func TestConsolePreflightOfAnAgent(t *testing.T) {
	ctx := context.Background()
	p, _, _ := newConsolePreflight(t, nil)
	spec := kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"}
	checks, err := p.Preflight(ctx, "edge-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	if b := Blocked(checks); len(b) > 0 {
		t.Fatalf("blocked: %+v", b)
	}
	m := checkMap(checks)
	if checks[0].Check != CheckAgentTarget || m[CheckTarget].Message != "0.5.0 → 0.6.0 (Minor)." ||
		!strings.Contains(m[CheckInstallerNode].Message, "edge-1-server") || !strings.Contains(m[CheckDataRollback].Message, "no database") {
		t.Errorf("checks = %+v", checks)
	}
	if _, ok := m[CheckAgentSkew]; ok {
		t.Error("an agent's preflight checks agent skew")
	}

	// Never past the console.
	checks, _ = p.Preflight(ctx, "edge-1", kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.7.0-rc.1"})
	if c := checkMap(checks)[CheckAgentTarget]; c.OK || !strings.Contains(c.Message, "upgrade it to 0.7.0-rc.1 first") {
		t.Errorf("agent target = %+v", c)
	}
	// A console on the edge channel takes its agents along: the agent's
	// own channel (it has no update settings) is not checked.
	p.Version = "0.7.0-rc.1"
	checks, _ = p.Preflight(ctx, "edge-1", kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.7.0-rc.1"})
	if b := Blocked(checks); len(b) > 0 {
		t.Errorf("an edge release of the console blocked for its agent: %+v", b)
	}
}

func TestConsolePreflightOfAnAgentReadsItsCluster(t *testing.T) {
	busy := &kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "edge-1-workers"}, Spec: kwerftv1.NodePoolSpec{Cluster: "edge-1"},
		Status: kwerftv1.NodePoolStatus{Nodes: []kwerftv1.PoolNode{{Name: "edge-1-workers-a", Phase: PoolNodeJoining}}}}
	p, _, asked := newConsolePreflight(t, []client.Object{busy}, testNode("edge-1-worker", false, nil))
	m := checkMap(mustPreflight(t, p, "edge-1", kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"}))
	if c := m[CheckNodesReady]; c.OK || !strings.Contains(c.Message, "edge-1-worker") {
		t.Errorf("nodes = %+v", c)
	}
	// Node pools live in the management cluster.
	if c := m[CheckNoOtherOperation]; c.OK || !strings.Contains(c.Message, "edge-1-workers") {
		t.Errorf("operations = %+v", c)
	}

	// Kubernetes: that cluster's API server, the agent's release.
	m = checkMap(mustPreflight(t, p, "edge-1", kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKubernetes, Version: "v1.37.2+k3s1"}))
	if len(*asked) != 1 || (*asked)[0] != "edge-1" {
		t.Errorf("API server asked for %v", *asked)
	}
	if c := m[CheckKwerftSupports]; !c.OK || !strings.Contains(c.Message, "Kwerft 0.5.0") {
		t.Errorf("Kwerft supports = %+v", c)
	}
	if _, ok := m[CheckAgentTarget]; ok {
		t.Error("a Kubernetes upgrade got the agent target check")
	}
}

func mustPreflight(t *testing.T, p *ConsolePreflight, cluster string, spec kwerftv1.UpgradeSpec) []kwerftv1.UpgradeCheck {
	t.Helper()
	checks, err := p.Preflight(context.Background(), cluster, spec)
	if err != nil {
		t.Fatal(err)
	}
	return checks
}

func TestConsolePreflightKubernetesTarget(t *testing.T) {
	ctx := context.Background()
	p, releases, _ := newConsolePreflight(t, nil)
	k := p.KubernetesTarget(ctx, "v0.5.0", "v1.37.1+k3s1")
	if k == nil || k.Version != "v1.37.2+k3s1" || k.Kind != "Patch" || !k.Allowed || k.Component != kwerftv1.UpgradeKubernetes {
		t.Fatalf("target = %+v", k)
	}
	calls := releases.calls.Load()
	if p.KubernetesTarget(ctx, "0.5.0", "v1.37.1+k3s1") == nil || releases.calls.Load() != calls {
		t.Error("the manifest was read again")
	}
	if p.KubernetesTarget(ctx, "0.5.0", "v1.37.2+k3s1") != nil {
		t.Error("offered what runs already")
	}
	if p.KubernetesTarget(ctx, "0.4.9", "v1.37.1+k3s1") != nil || p.KubernetesTarget(ctx, "0.1.0-dev", "v1.37.1+k3s1") != nil {
		t.Error("a target without a manifest")
	}
}
