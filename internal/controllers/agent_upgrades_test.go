package controllers

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
)

// The agent clusters' fleet against fake APIs: the management cluster and
// one per agent cluster.

type fakeClusterClients map[string]client.Client

func (f fakeClusterClients) For(_ context.Context, name string) (client.Client, error) {
	if c, ok := f[name]; ok {
		return c, nil
	}
	return nil, clusters.ErrUnavailable
}

type fleetEnv struct {
	mgmt    client.Client
	remotes fakeClusterClients
	r       *AgentUpgradesReconciler
}

func newFleetEnv(t *testing.T, console *kwerftv1.Upgrade, agents map[string]string) *fleetEnv {
	t.Helper()
	objs := []client.Object{}
	if console != nil {
		objs = append(objs, console)
	}
	e := &fleetEnv{remotes: fakeClusterClients{}}
	for name, version := range agents {
		cl := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: kwerftv1.ClusterStatus{Phase: ClusterConnected, AgentVersion: version}}
		if version == "" {
			cl.Status = kwerftv1.ClusterStatus{Phase: ClusterDisconnected, AgentVersion: "0.5.0"}
		}
		objs = append(objs, cl)
		e.remotes[name] = fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(&kwerftv1.Upgrade{}).Build()
	}
	e.mgmt = fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(&kwerftv1.Upgrade{}, &kwerftv1.Cluster{}).WithObjects(objs...).Build()
	e.r = &AgentUpgradesReconciler{Client: e.mgmt, Clusters: e.remotes, Version: "0.6.0"}
	return e
}

// member plays the API: the owner's held Upgrade in an agent cluster.
func (e *fleetEnv) member(t *testing.T, cluster, fleet, after string, order int) string {
	t.Helper()
	u := FleetMember(fleet, after, order, "0.6.0", "alice@example.com")
	u.Name = u.GenerateName + cluster
	if err := e.remotes[cluster].Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u.Name
}

func (e *fleetEnv) remote(t *testing.T, cluster, name string) *kwerftv1.Upgrade {
	t.Helper()
	var u kwerftv1.Upgrade
	if err := e.remotes[cluster].Get(context.Background(), client.ObjectKey{Name: name}, &u); err != nil {
		t.Fatal(err)
	}
	return &u
}

func (e *fleetEnv) setRemotePhase(t *testing.T, cluster, name string, phase kwerftv1.UpgradePhase) {
	t.Helper()
	u := e.remote(t, cluster, name)
	u.Status.Phase = phase
	if err := e.remotes[cluster].Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func (e *fleetEnv) setConsolePhase(t *testing.T, name string, phase kwerftv1.UpgradePhase) {
	t.Helper()
	var u kwerftv1.Upgrade
	if err := e.mgmt.Get(context.Background(), client.ObjectKey{Name: name}, &u); err != nil {
		t.Fatal(err)
	}
	u.Status.Phase = phase
	if err := e.mgmt.Status().Update(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
}

func (e *fleetEnv) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := e.r.Reconcile(context.Background(), fleetKey)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func consoleUpgrade(name string, phase kwerftv1.UpgradePhase) *kwerftv1.Upgrade {
	return &kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:   kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"},
		Status: kwerftv1.UpgradeStatus{Phase: phase}}
}

func TestFleetWaitsForTheConsoleThenGoesOneByOne(t *testing.T) {
	const console = "kwerft-0.6.0-x7k2p"
	e := newFleetEnv(t, consoleUpgrade(console, kwerftv1.UpgradeRunning), map[string]string{"edge-1": "0.5.2", "edge-2": "0.5.0"})
	m1 := e.member(t, "edge-1", console, console, 0)
	m2 := e.member(t, "edge-2", console, console, 1)
	if u := e.remote(t, "edge-1", m1); !strings.Contains(u.Annotations[kwerftv1.AnnotationHold], "console's upgrade "+console) ||
		u.Labels[kwerftv1.LabelFleet] != console || u.GenerateName != "kwerft-0.6.0-" || u.Annotations[kwerftv1.AnnotationRequestedBy] != "alice@example.com" {
		t.Fatalf("member = %+v", u.ObjectMeta)
	}

	// The console upgrades: both stay held.
	if res := e.reconcile(t); res.RequeueAfter == 0 {
		t.Error("no requeue while the fleet is unfinished")
	}
	if e.remote(t, "edge-1", m1).Annotations[kwerftv1.AnnotationHold] == "" {
		t.Fatal("released before the console's upgrade finished")
	}

	// The console succeeded: the first goes, the second waits for it.
	e.setConsolePhase(t, console, kwerftv1.UpgradeSucceeded)
	e.reconcile(t)
	if e.remote(t, "edge-1", m1).Annotations[kwerftv1.AnnotationHold] != "" {
		t.Fatal("edge-1 not released")
	}
	if h := e.remote(t, "edge-2", m2).Annotations[kwerftv1.AnnotationHold]; h != "Waiting for its turn: edge-1 upgrades first." {
		t.Fatalf("edge-2 hold = %q", h)
	}
	e.setRemotePhase(t, "edge-1", m1, kwerftv1.UpgradeRunning)
	e.reconcile(t)
	if e.remote(t, "edge-2", m2).Annotations[kwerftv1.AnnotationHold] == "" {
		t.Fatal("two at once")
	}
	var c kwerftv1.Upgrade
	if err := e.mgmt.Get(context.Background(), client.ObjectKey{Name: console}, &c); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, ConditionAgentClusters); cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Message != "Agent clusters (edge-1: Running, edge-2: waiting)." {
		t.Errorf("condition = %+v", cond)
	}

	// edge-1 succeeded: edge-2 goes, and the fleet is done with it.
	e.setRemotePhase(t, "edge-1", m1, kwerftv1.UpgradeSucceeded)
	e.reconcile(t)
	if e.remote(t, "edge-2", m2).Annotations[kwerftv1.AnnotationHold] != "" {
		t.Fatal("edge-2 not released after edge-1")
	}
	e.setRemotePhase(t, "edge-2", m2, kwerftv1.UpgradeSucceeded)
	if res := e.reconcile(t); res.RequeueAfter != 0 {
		t.Errorf("requeue after the fleet finished: %v", res)
	}
	if err := e.mgmt.Get(context.Background(), client.ObjectKey{Name: console}, &c); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, ConditionAgentClusters); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Finished" {
		t.Errorf("condition = %+v", cond)
	}
}

// A member that fails or rolls back stops the rest of the fleet: the held
// ones are cancelled, naming the cluster that failed.
func TestFleetStopsWhenAMemberFails(t *testing.T) {
	for _, phase := range []kwerftv1.UpgradePhase{kwerftv1.UpgradeRolledBack, kwerftv1.UpgradeFailed} {
		const console = "kwerft-0.6.0-x7k2p"
		e := newFleetEnv(t, consoleUpgrade(console, kwerftv1.UpgradeSucceeded), map[string]string{"edge-1": "0.5.2", "edge-2": "0.5.0", "edge-3": "0.5.0"})
		m1 := e.member(t, "edge-1", console, console, 0)
		m2 := e.member(t, "edge-2", console, console, 1)
		m3 := e.member(t, "edge-3", console, console, 2)
		e.reconcile(t)
		e.setRemotePhase(t, "edge-1", m1, phase)
		if res := e.reconcile(t); res.RequeueAfter == 0 {
			t.Errorf("%s: no requeue while the cancelled members finish", phase)
		}
		for cluster, name := range map[string]string{"edge-2": m2, "edge-3": m3} {
			u := e.remote(t, cluster, name)
			if u.Annotations[kwerftv1.AnnotationHold] == "" {
				t.Errorf("%s: %s released after edge-1 ended %s", phase, cluster, phase)
			}
			if by, want := u.Annotations[kwerftv1.AnnotationCancelRequested], "of edge-1 ended "+string(phase); !strings.Contains(by, want) {
				t.Errorf("%s: %s cancel = %q, want %q", phase, cluster, by, want)
			}
		}
		e.setRemotePhase(t, "edge-2", m2, kwerftv1.UpgradeCancelled)
		e.setRemotePhase(t, "edge-3", m3, kwerftv1.UpgradeCancelled)
		if res := e.reconcile(t); res.RequeueAfter != 0 {
			t.Errorf("%s: requeue after the fleet stopped: %v", phase, res)
		}
		var c kwerftv1.Upgrade
		if err := e.mgmt.Get(context.Background(), client.ObjectKey{Name: console}, &c); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(c.Status.Conditions, ConditionAgentClusters)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Stopped" || !strings.Contains(cond.Message, "Stopped: edge-1 ended "+string(phase)) {
			t.Errorf("%s: condition = %+v", phase, cond)
		}
	}
}

func TestFleetCancelledWhenTheConsoleUpgradeFails(t *testing.T) {
	const console = "kwerft-0.6.0-x7k2p"
	e := newFleetEnv(t, consoleUpgrade(console, kwerftv1.UpgradeRolledBack), map[string]string{"edge-1": "0.5.2"})
	m1 := e.member(t, "edge-1", console, console, 0)
	e.reconcile(t)
	if by := e.remote(t, "edge-1", m1).Annotations[kwerftv1.AnnotationCancelRequested]; !strings.Contains(by, "ended RolledBack") {
		t.Errorf("cancel = %q", by)
	}
	// Gone altogether: cancelled too.
	e = newFleetEnv(t, nil, map[string]string{"edge-1": "0.5.2"})
	m1 = e.member(t, "edge-1", console, console, 0)
	e.reconcile(t)
	if by := e.remote(t, "edge-1", m1).Annotations[kwerftv1.AnnotationCancelRequested]; !strings.Contains(by, "is gone") {
		t.Errorf("cancel = %q", by)
	}
}

func TestFleetWithoutAConsoleUpgradeSkipsTheDisconnected(t *testing.T) {
	e := newFleetEnv(t, nil, map[string]string{"away": "", "edge-2": "0.5.0"})
	fleet := "fleet-0123456789"
	away := e.member(t, "away", fleet, "", 0)
	m2 := e.member(t, "edge-2", fleet, "", 1)
	e.reconcile(t)
	if e.remote(t, "edge-2", m2).Annotations[kwerftv1.AnnotationHold] != "" {
		t.Error("edge-2 waits for a disconnected cluster")
	}
	if e.remote(t, "away", away).Annotations[kwerftv1.AnnotationHold] == "" {
		t.Error("a disconnected cluster was touched")
	}
}

func TestFleetNeverStartsAnAgentPastTheConsole(t *testing.T) {
	e := newFleetEnv(t, nil, map[string]string{"edge-1": "0.5.2"})
	e.r.Version = "0.5.9"
	m1 := e.member(t, "edge-1", "fleet-0123456789", "", 0)
	e.reconcile(t)
	u := e.remote(t, "edge-1", m1)
	if u.Annotations[kwerftv1.AnnotationHold] == "" || !strings.Contains(u.Annotations[kwerftv1.AnnotationCancelRequested], "upgrade it to 0.6.0 first") {
		t.Errorf("annotations = %v", u.Annotations)
	}
}
