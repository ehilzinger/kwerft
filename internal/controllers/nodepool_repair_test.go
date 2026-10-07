// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Repairs of broken workers and the Nodes of deleted servers (found on
// kwerft-dev-test, 2026-10-05: two workers replaced at once, their Nodes
// left behind).

// setNodeReady sets a node's Ready condition, changed at since.
func setNodeReady(t *testing.T, name string, ready bool, since time.Time) {
	t.Helper()
	ctx := context.Background()
	var n corev1.Node
	if err := k8s.Get(ctx, client.ObjectKey{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionUnknown
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status,
		LastTransitionTime: metav1.NewTime(since), LastHeartbeatTime: metav1.NewTime(since)}}
	if err := k8s.Status().Update(ctx, &n); err != nil {
		t.Fatal(err)
	}
}

func (e *poolEnv) serverNames(cluster, pool string) []string {
	var out []string
	for _, s := range e.poolServers(cluster, pool) {
		out = append(out, s.Name)
	}
	return out
}

func poolNodeMessage(p *kwerftv1.NodePool, name string) string {
	for _, n := range p.Status.Nodes {
		if n.Name == name {
			return n.Message
		}
	}
	return ""
}

// TestNodePoolDeletesTheNodeOfAReplacedServer reproduces the Node a
// replaced worker left behind: the pool deleted the Node, then the server,
// and k3s registered the Node again while the server was shutting down.
// It must go once the server is gone, and only then.
func TestNodePoolDeletesTheNodeOfAReplacedServer(t *testing.T) {
	e := newPoolEnv(t, "np7")
	joinMaterial(t, "np7")
	e.r.RepairAfter = time.Minute
	createPool(t, "np7-workers", kwerftv1.NodePoolSpec{Cluster: "np7", ServerType: "cx23", Location: "nbg1", Count: 2})
	// The rest of the cluster is fine (a broken node is not the majority).
	addNode(t, "np7-cp", map[string]string{LabelControlPlane: "true"}, true)
	addNode(t, "np7-dedi", nil, true)
	e.pass(t, "np7-workers")
	workers := e.join(t, "np7", "np7-workers", false)
	e.pass(t, "np7-workers")

	broken := workers[0]
	setNodeReady(t, broken, false, time.Now().Add(-10*time.Minute))
	e.pass(t, "np7-workers")
	if del := e.f.DeletedServers(); len(del) != 1 || del[0] != broken {
		t.Fatalf("deleted = %v, want %s", del, broken)
	}
	if !nodeGone(broken) {
		t.Fatal("the removal did not delete the node")
	}
	// k3s registers it again before the server is off.
	addNode(t, broken, map[string]string{hetzner.LabelPool: "np7-workers"}, false)

	// A Node whose server is still being deleted stays until it is gone;
	// a Ready one, and another pool's, are never touched.
	ctx := context.Background()
	going := e.f.PutServer(hetzner.Server{Name: "np7-workers-going", Status: hetzner.ServerDeleting,
		Labels: map[string]string{hetzner.LabelCluster: "np7", hetzner.LabelPool: "np7-workers"}})
	addNode(t, "np7-workers-going", map[string]string{hetzner.LabelPool: "np7-workers"}, false)
	addNode(t, "np7-workers-alive", map[string]string{hetzner.LabelPool: "np7-workers"}, true)
	addNode(t, "np7-other-gone", map[string]string{hetzner.LabelPool: "np7-other"}, false)

	e.pass(t, "np7-workers")
	if !nodeGone(broken) {
		t.Fatal("the node of the deleted server was left behind")
	}
	if nodeGone("np7-workers-going") || nodeGone("np7-workers-alive") || nodeGone("np7-other-gone") {
		t.Fatal("deleted a node whose server is not gone, a Ready node or another pool's node")
	}
	if err := e.f.Client().DeleteServer(ctx, going); err != nil {
		t.Fatal(err)
	}
	e.pass(t, "np7-workers")
	if !nodeGone("np7-workers-going") || nodeGone("np7-workers-alive") {
		t.Fatal("the node of a server deleted by hand stayed, or a Ready node went")
	}
	// The broken worker was replaced.
	if names := e.serverNames("np7", "np7-workers"); len(names) != 2 || names[0] != workers[1] {
		t.Fatalf("servers = %v", names)
	}
}

// TestNodePoolRepairsOneAtATime: broken workers are replaced one after
// another, and not at all while most of the cluster's nodes are NotReady
// (which points at the cluster or its network, as when both workers of
// kwerft-dev-test went NotReady together and were replaced at once).
func TestNodePoolRepairsOneAtATime(t *testing.T) {
	e := newPoolEnv(t, "np8")
	joinMaterial(t, "np8")
	e.r.RepairAfter = time.Minute
	createPool(t, "np8-workers", kwerftv1.NodePoolSpec{Cluster: "np8", ServerType: "cx23", Location: "nbg1", Count: 2})
	addNode(t, "np8-cp", map[string]string{LabelControlPlane: "true"}, true)
	e.pass(t, "np8-workers")
	workers := e.join(t, "np8", "np8-workers", false)
	e.pass(t, "np8-workers")

	// Both workers stop reporting within a minute: 2 of 3 nodes NotReady.
	setNodeReady(t, workers[0], false, time.Now().Add(-20*time.Minute))
	setNodeReady(t, workers[1], false, time.Now().Add(-19*time.Minute))
	p := e.passes(t, "np8-workers", 2)
	if del := e.f.DeletedServers(); len(del) != 0 {
		t.Fatalf("replaced while most of the cluster is NotReady: %v", del)
	}
	if msg := poolNodeMessage(p, workers[0]); !strings.Contains(msg, "Not replaced: 2 of the cluster's 3 nodes are NotReady") {
		t.Fatalf("message = %q", msg)
	}
	if reason, msg := readyCondition(p); reason != "Progressing" || !strings.Contains(msg, "broken servers are not replaced") {
		t.Fatalf("ready = %s %q", reason, msg)
	}

	// The rest of the cluster is fine: the longest broken goes first, alone.
	addNode(t, "np8-dedi-1", nil, true)
	addNode(t, "np8-dedi-2", nil, true)
	p = e.passes(t, "np8-workers", 2)
	if del := e.f.DeletedServers(); len(del) != 1 || del[0] != workers[0] {
		t.Fatalf("deleted = %v, want only %s", del, workers[0])
	}
	if msg := poolNodeMessage(p, workers[1]); !strings.Contains(msg, "Replaced once the server being replaced now is done") {
		t.Fatalf("message = %q", msg)
	}
	e.passes(t, "np8-workers", 2)
	if del := e.f.DeletedServers(); len(del) != 1 {
		t.Fatalf("the second went while the first one's replacement joins: %v", del)
	}
	// The replacement joined: now the second.
	e.join(t, "np8", "np8-workers", false)
	e.passes(t, "np8-workers", 2)
	if del := e.f.DeletedServers(); len(del) != 2 || del[1] != workers[1] {
		t.Fatalf("deleted = %v", del)
	}
}

func TestServerPrefix(t *testing.T) {
	for _, c := range []struct{ cluster, pool, want string }{
		{"local", "local-workers", "local-workers"}, // as the console names pools
		{"np1", "workers", "np1-workers"},           // kubectl
		{"edge.1", "edge.1-pool", "edge-1-pool"},
		{"local", "localworkers", "local-localworkers"},
	} {
		if got := serverPrefix(c.cluster, c.pool); got != c.want {
			t.Errorf("serverPrefix(%q, %q) = %q, want %q", c.cluster, c.pool, got, c.want)
		}
	}
}
