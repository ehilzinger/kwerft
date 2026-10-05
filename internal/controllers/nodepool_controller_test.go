package controllers

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
	"github.com/ehilzinger/kwerft/internal/jointoken"
)

// Node pools against a fake Hetzner Cloud and the test API server, which
// stands in for every pool's cluster. Nodes "join" when the test creates
// their Node objects; the reconciler is called directly, pass by pass.

type poolEnv struct {
	f      *hetznertest.Server
	r      *NodePoolReconciler
	clock  *offsetClock
	signer *jointoken.Signer
	n      atomic.Int32
}

const testConsoleURL = "https://console.example.com"

func newPoolEnv(t *testing.T, clusterNames ...string) *poolEnv {
	t.Helper()
	if k8s == nil {
		t.Skip("envtest not available")
	}
	e := &poolEnv{f: hetznertest.New(t, "cloud-token"), clock: &offsetClock{}, signer: jointoken.NewSigner([]byte("test data key, 32 bytes long...."))}
	e.f.FakeNodes()
	var nets []hetzner.NetworkRef
	cc := StaticClients{}
	for _, c := range clusterNames {
		nets = append(nets, hetzner.NetworkRef{Name: hetzner.NetworkName(c), IPRange: "10.0.0.0/16"})
		cc[c] = k8s
	}
	e.f.FakeClusterNetworks(nets...)
	e.f.AddSSHKey("ops", nil)
	e.signer.Now = e.clock.Now
	e.r = &NodePoolReconciler{
		Client: k8s, APIReader: k8s, Namespace: GatewayNamespace,
		HCloud:     func(context.Context) (*hetzner.Client, error) { return e.f.Client(), nil },
		Clusters:   cc,
		JoinTokens: e.signer,
		ConsoleURL: func() string { return testConsoleURL },
		Now:        e.clock.Now,
		Suffix:     func() string { return fmt.Sprintf("n%04d", e.n.Add(1)) },
	}
	return e
}

func joinMaterial(t *testing.T, cluster string) {
	t.Helper()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: JoinSecretName(cluster), Namespace: GatewayNamespace},
		StringData: map[string]string{"server": "https://10.0.0.2:6443", "token": "K10cafe::server:k3s-secret"}}
	if err := k8s.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), sec) })
}

func createPool(t *testing.T, name string, spec kwerftv1.NodePoolSpec) *kwerftv1.NodePool {
	t.Helper()
	pool := &kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		var p kwerftv1.NodePool
		if k8s.Get(ctx, client.ObjectKey{Name: name}, &p) == nil {
			p.Finalizers = nil
			_ = k8s.Update(ctx, &p)
			_ = k8s.Delete(ctx, &p)
		}
	})
	return pool
}

// pass runs one reconcile and returns the pool as it is afterwards.
func (e *poolEnv) pass(t *testing.T, name string) (*kwerftv1.NodePool, ctrl.Result) {
	t.Helper()
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
	// The manager's Cluster reconciler writes control-plane pools too; a
	// conflict is requeued in production, retried here.
	for try := 0; apierrors.IsConflict(err) && try < 5; try++ {
		res, err = e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
	}
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	var p kwerftv1.NodePool
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, res
		}
		t.Fatal(err)
	}
	return &p, res
}

func (e *poolEnv) passes(t *testing.T, name string, n int) *kwerftv1.NodePool {
	t.Helper()
	var p *kwerftv1.NodePool
	for range n {
		p, _ = e.pass(t, name)
	}
	return p
}

func readyCondition(p *kwerftv1.NodePool) (string, string) {
	c := meta.FindStatusCondition(p.Status.Conditions, ConditionReady)
	if c == nil {
		return "", ""
	}
	return c.Reason, c.Message
}

// poolServers are the fake's servers of a pool.
func (e *poolEnv) poolServers(cluster, pool string) []hetzner.Server {
	var out []hetzner.Server
	for _, s := range e.f.CloudServers() {
		if s.Labels[hetzner.LabelCluster] == cluster && s.Labels[hetzner.LabelPool] == pool {
			out = append(out, s)
		}
	}
	return out
}

// join registers a Ready Node for every running server of the pool that
// has none yet, as k3s would after cloud-init ran the installer.
func (e *poolEnv) join(t *testing.T, cluster, pool string, controlPlane bool) []string {
	t.Helper()
	e.f.RunAllServers()
	var names []string
	for _, s := range e.poolServers(cluster, pool) {
		labels := map[string]string{hetzner.LabelPool: pool}
		if controlPlane {
			labels[LabelControlPlane] = "true"
		}
		addNode(t, s.Name, labels, true)
		names = append(names, s.Name)
	}
	return names
}

func addNode(t *testing.T, name string, labels map[string]string, ready bool) *corev1.Node {
	t.Helper()
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if err := k8s.Create(ctx, n); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return n
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}) })
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status, LastTransitionTime: metav1.Now(), LastHeartbeatTime: metav1.Now()}}
	if err := k8s.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	return n
}

// addPod puts a pod on a node; running ones are Ready (PDBs count them).
func addPod(t *testing.T, ns, name, node string, labels map[string]string, running ...bool) {
	t.Helper()
	ctx := context.Background()
	_ = k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	zero := int64(0)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{NodeName: node, TerminationGracePeriodSeconds: &zero, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), p) })
	if len(running) > 0 && running[0] {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := k8s.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
}

func podGone(t *testing.T, ns, name string) bool {
	t.Helper()
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &corev1.Pod{})
	return apierrors.IsNotFound(err)
}

func nodeGone(name string) bool {
	return apierrors.IsNotFound(k8s.Get(context.Background(), client.ObjectKey{Name: name}, &corev1.Node{}))
}

var joinTokenRE = regexp.MustCompile(`'(kwft_join_[A-Za-z0-9_.-]+)'`)

func TestNodePoolGrowJoinShrinkDelete(t *testing.T) {
	e := newPoolEnv(t, "np1")
	joinMaterial(t, "np1")
	ctx := context.Background()
	createPool(t, "np1-workers", kwerftv1.NodePoolSpec{Cluster: "np1", Role: kwerftv1.NodeWorker, ServerType: "cx23", Location: "fsn1", Count: 2,
		Labels: map[string]string{"tier": "web"}})

	p, res := e.pass(t, "np1-workers")
	servers := e.poolServers("np1", "np1-workers")
	if len(servers) != 2 || res.RequeueAfter > 30*time.Second {
		t.Fatalf("servers = %d, requeue %v", len(servers), res.RequeueAfter)
	}
	for _, s := range servers {
		if !strings.HasPrefix(s.Name, "np1-np1-workers-n") || s.Image.Name != "ubuntu-26.04" || s.PlacementGroup == nil ||
			len(s.PrivateNet) != 1 || s.Labels[hetzner.LabelRole] != "worker" {
			t.Fatalf("server = %+v", s)
		}
		ud := e.f.UserData(s.ID)
		for _, want := range []string{"#cloud-config", "--join' 'https://console.example.com' '--token'", "'--role' 'worker'",
			"'--node-label' 'kwerft.dev/pool=np1-workers'", "'--node-label' 'tier=web'", "'--yes'"} {
			if !strings.Contains(ud, want) {
				t.Errorf("user data lacks %q:\n%s", want, ud)
			}
		}
		if strings.Contains(ud, "k3s-secret") || strings.Contains(ud, "node-taint") {
			t.Errorf("user data carries the k3s token or a taint:\n%s", ud)
		}
		m := joinTokenRE.FindStringSubmatch(ud)
		if m == nil {
			t.Fatalf("no join token in user data")
		}
		claims, err := e.signer.Verify(m[1])
		if err != nil || claims.Node != s.Name || claims.Cluster != "np1" || claims.Role != jointoken.RoleWorker {
			t.Fatalf("claims = %+v, %v", claims, err)
		}
	}
	if len(p.Status.Nodes) != 2 || p.Status.Nodes[0].Phase != PoolNodeCreating || p.Status.Desired != 2 {
		t.Fatalf("status = %+v", p.Status)
	}
	if pgs := e.f.PlacementGroupsNow(); len(pgs) != 1 || pgs[0].Name != "kwerft-np1-np1-workers" || len(pgs[0].Servers) != 2 {
		t.Fatalf("placement groups = %+v", pgs)
	}

	// Booted: joining. Nodes registered: Ready, and labelled.
	e.f.RunAllServers()
	p, _ = e.pass(t, "np1-workers")
	if p.Status.Nodes[0].Phase != PoolNodeJoining {
		t.Fatalf("phase = %s", p.Status.Nodes[0].Phase)
	}
	names := e.join(t, "np1", "np1-workers", false)
	p, _ = e.pass(t, "np1-workers")
	if reason, msg := readyCondition(p); reason != "Ready" || p.Status.ReadyNodes != 2 {
		t.Fatalf("ready = %s %q, %+v", reason, msg, p.Status)
	}
	var n corev1.Node
	if err := k8s.Get(ctx, client.ObjectKey{Name: names[0]}, &n); err != nil || n.Labels["tier"] != "web" || n.Labels[hetzner.LabelRole] != "worker" {
		t.Fatalf("node labels = %v, %v", n.Labels, err)
	}
	if e.f.ServerCreates() != 2 {
		t.Fatalf("creates = %d", e.f.ServerCreates())
	}

	// Shrink to one: the newest goes, drained first.
	addPod(t, "np1-apps", "web-1", names[1], nil)
	p.Spec.Count = 1
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.passes(t, "np1-workers", 3)
	if !podGone(t, "np1-apps", "web-1") || !nodeGone(names[1]) {
		t.Fatal("the removed node was not drained and deleted")
	}
	if del := e.f.DeletedServers(); !slices.Equal(del, []string{names[1]}) {
		t.Fatalf("deleted = %v", del)
	}
	p, _ = e.pass(t, "np1-workers")
	if len(p.Status.Nodes) != 1 || p.Status.ReadyNodes != 1 {
		t.Fatalf("status = %+v", p.Status)
	}

	// Deleting the pool removes its last server, then the placement group.
	if err := k8s.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if p, _ = e.pass(t, "np1-workers"); p == nil {
			break
		}
	}
	if p != nil {
		t.Fatalf("pool still there: %+v", p.Status)
	}
	if len(e.f.CloudServers()) != 0 || len(e.f.PlacementGroupsNow()) != 0 {
		t.Fatalf("left behind: %+v %+v", e.f.CloudServers(), e.f.PlacementGroupsNow())
	}
}

func TestNodePoolOnlyDeletesItsOwnServers(t *testing.T) {
	e := newPoolEnv(t, "np2")
	joinMaterial(t, "np2")
	ctx := context.Background()
	// Look-alikes: same name prefix but no pool label, and another pool's.
	byHand := e.f.PutServer(hetzner.Server{Name: "np2-np2-pool-byhand", Labels: map[string]string{hetzner.LabelCluster: "np2"}})
	other := e.f.PutServer(hetzner.Server{Name: "np2-other-x", Labels: map[string]string{hetzner.LabelCluster: "np2", hetzner.LabelPool: "other"}})
	createPool(t, "np2-pool", kwerftv1.NodePoolSpec{Cluster: "np2", ServerType: "cx23", Location: "nbg1", Count: 1})
	e.pass(t, "np2-pool")
	e.join(t, "np2", "np2-pool", false)
	p, _ := e.pass(t, "np2-pool")
	if err := k8s.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if p, _ = e.pass(t, "np2-pool"); p == nil {
			break
		}
	}
	left := map[int64]bool{}
	for _, s := range e.f.CloudServers() {
		left[s.ID] = true
	}
	if !left[byHand] || !left[other] || len(left) != 2 {
		t.Fatalf("servers left = %v", left)
	}
}

func TestNodePoolReplacesServerTypeOneAtATime(t *testing.T) {
	e := newPoolEnv(t, "np3")
	joinMaterial(t, "np3")
	ctx := context.Background()
	createPool(t, "np3-pool", kwerftv1.NodePoolSpec{Cluster: "np3", ServerType: "cx23", Location: "fsn1", Count: 2})
	e.pass(t, "np3-pool")
	old := e.join(t, "np3", "np3-pool", false)
	p, _ := e.pass(t, "np3-pool")

	p.Spec.ServerType = "cx33"
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.passes(t, "np3-pool", 2)
	if s := e.poolServers("np3", "np3-pool"); len(s) != 3 || s[2].ServerType.Name != "cx33" {
		t.Fatalf("want one extra cx33 server first, have %+v", s)
	}
	e.join(t, "np3", "np3-pool", false)
	e.passes(t, "np3-pool", 3)
	if del := e.f.DeletedServers(); len(del) != 1 || !slices.Contains(old, del[0]) {
		t.Fatalf("deleted = %v", del)
	}
	// And the second one the same way.
	for range 3 {
		e.passes(t, "np3-pool", 2)
		e.join(t, "np3", "np3-pool", false)
	}
	e.passes(t, "np3-pool", 3)
	types := map[string]int{}
	for _, s := range e.poolServers("np3", "np3-pool") {
		types[s.ServerType.Name]++
	}
	if types["cx33"] != 2 || types["cx23"] != 0 {
		t.Fatalf("types = %v (deleted %v)", types, e.f.DeletedServers())
	}
}

func TestControlPlanePool(t *testing.T) {
	e := newPoolEnv(t, "np4")
	joinMaterial(t, "np4")
	e.r.EtcdRemoveTimeout = time.Hour
	ctx := context.Background()
	createPool(t, "np4-cp", kwerftv1.NodePoolSpec{Cluster: "np4", Role: kwerftv1.NodeControlPlane, ServerType: "cx23", Location: "fsn1", Count: 2})

	// Two control-plane nodes: refused, nothing created.
	p, _ := e.pass(t, "np4-cp")
	if reason, _ := readyCondition(p); reason != "EvenControlPlane" || len(e.f.CloudServers()) != 0 {
		t.Fatalf("reason %q, servers %d", reason, len(e.f.CloudServers()))
	}

	// Three: one server at a time, each a k3s server.
	p.Spec.Count = 3
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		e.passes(t, "np4-cp", 2)
		s := e.poolServers("np4", "np4-cp")
		if len(s) != want {
			t.Fatalf("after joining %d: %d servers", want-1, len(s))
		}
		if ud := e.f.UserData(s[want-1].ID); !strings.Contains(ud, "'--role' 'control-plane'") {
			t.Fatalf("user data:\n%s", ud)
		}
		e.join(t, "np4", "np4-cp", true)
	}
	p, _ = e.pass(t, "np4-cp")
	if p.Status.ReadyNodes != 3 {
		t.Fatalf("status = %+v", p.Status)
	}

	// Down to one: one at a time, each after k3s removed its etcd member.
	p.Spec.Count = 1
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	removed := 0
	for range 12 {
		e.pass(t, "np4-cp")
		var nodes corev1.NodeList
		if err := k8s.List(ctx, &nodes, client.MatchingLabels{hetzner.LabelPool: "np4-cp"}); err != nil {
			t.Fatal(err)
		}
		draining := 0
		for i := range nodes.Items {
			n := &nodes.Items[i]
			if n.Annotations[annotationEtcdRemove] == "true" {
				draining++
				// k3s's etcd member controller answers.
				patch := client.MergeFrom(n.DeepCopy())
				n.Annotations[annotationEtcdRemoved] = n.Name
				if err := k8s.Patch(ctx, n, patch); err != nil {
					t.Fatal(err)
				}
			}
		}
		if draining > 1 {
			t.Fatal("two control-plane nodes leaving at once")
		}
		removed = len(e.f.DeletedServers())
	}
	if removed != 2 || len(e.poolServers("np4", "np4-cp")) != 1 {
		t.Fatalf("removed %d, left %d", removed, len(e.poolServers("np4", "np4-cp")))
	}

	// The last one stays.
	p, _ = e.pass(t, "np4-cp")
	p.Spec.Count = 0
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	p, _ = e.pass(t, "np4-cp")
	if reason, _ := readyCondition(p); reason != "LastControlPlane" || len(e.poolServers("np4", "np4-cp")) != 1 {
		t.Fatalf("reason %q", reason)
	}
}

func TestBuildPoolScalesWithBuilds(t *testing.T) {
	e := newPoolEnv(t, "np5")
	joinMaterial(t, "np5")
	ctx := context.Background()
	// Other tests' build Jobs never finish here (no Job controller).
	if err := k8s.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(builds.Namespace), client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatal(err)
	}
	createPool(t, "np5-builds", kwerftv1.NodePoolSpec{Cluster: "np5", Role: kwerftv1.NodeBuilds, ServerType: "cx33", Location: "fsn1", Count: 2})
	t.Cleanup(func() {
		_ = k8s.Delete(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: BuildNodesConfigMap, Namespace: GatewayNamespace}})
	})

	// No build: no server, but builds now go to build nodes.
	p, _ := e.pass(t, "np5-builds")
	if len(e.f.CloudServers()) != 0 || p.Status.Desired != 0 {
		t.Fatalf("idle pool created servers")
	}
	if ok, err := buildNodesExist(ctx, k8s); !ok || err != nil {
		t.Fatalf("marker: %v %v", ok, err)
	}

	// A build Job waits: one server.
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "np5-build", Namespace: builds.Namespace, Labels: map[string]string{builds.LabelBuild: "x"}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "b", Image: "busybox"}}}}}}
	if err := k8s.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(context.Background(), job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	})
	p, _ = e.pass(t, "np5-builds")
	s := e.poolServers("np5", "np5-builds")
	if len(s) != 1 || p.Status.LastBuildAt == nil {
		t.Fatalf("servers = %d, status %+v", len(s), p.Status)
	}
	ud := e.f.UserData(s[0].ID)
	for _, want := range []string{"'--node-taint' 'kwerft.dev/builds=true:NoSchedule'", "'--node-label' 'kwerft.dev/builds=true'", "'--role' 'worker'"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data lacks %q", want)
		}
	}
	names := e.join(t, "np5", "np5-builds", false)
	e.pass(t, "np5-builds")
	var n corev1.Node
	if err := k8s.Get(ctx, client.ObjectKey{Name: names[0]}, &n); err != nil || !slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.MatchTaint(&TaintBuilds) }) {
		t.Fatalf("taints = %+v, %v", n.Spec.Taints, err)
	}

	// The build is over (its Job gone); the pool idles to zero after 15 minutes.
	if err := k8s.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatal(err)
	}
	e.passes(t, "np5-builds", 2)
	if len(e.poolServers("np5", "np5-builds")) != 1 {
		t.Fatal("scaled down before ScaleDownAfter")
	}
	e.clock.Advance(16 * time.Minute)
	e.passes(t, "np5-builds", 3)
	if len(e.poolServers("np5", "np5-builds")) != 0 || !nodeGone(names[0]) {
		t.Fatalf("still %d servers", len(e.poolServers("np5", "np5-builds")))
	}

	// Deleting the pool takes the marker away.
	p, _ = e.pass(t, "np5-builds")
	if err := k8s.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.passes(t, "np5-builds", 2)
	if ok, _ := buildNodesExist(ctx, k8s); ok {
		t.Fatal("marker left behind")
	}
}

func TestNewClusterBootstrapsThroughAgentMode(t *testing.T) {
	e := newPoolEnv(t) // the new cluster is not reachable yet
	e.f.FakeClusterNetworks(hetzner.NetworkRef{Name: hetzner.NetworkName("np6")})
	ctx := context.Background()
	cl := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "np6"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterHetznerCloud,
		HetznerCloud: &kwerftv1.HetznerClusterSpec{Location: "fsn1", ServerType: "cx23", ControlPlanes: 3}}}
	if err := k8s.Create(ctx, cl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), cl) })
	createPool(t, "np6-workers", kwerftv1.NodePoolSpec{Cluster: "np6", ServerType: "cx23", Location: "fsn1", Count: 2})

	// The Cluster reconciler adds the control-plane pool and the agent
	// token its first server bootstraps with.
	cpPool := ControlPlanePoolName("np6")
	var agent corev1.Secret
	eventually(t, func() error {
		var p kwerftv1.NodePool
		if err := k8s.Get(ctx, client.ObjectKey{Name: cpPool}, &p); err != nil {
			return err
		}
		if p.Spec.Count != 3 || roleOf(&p) != kwerftv1.NodeControlPlane {
			return fmt.Errorf("pool spec %+v", p.Spec)
		}
		return k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: AgentSecretName("np6")}, &agent)
	})
	token, consoleURL := string(agent.Data["token"]), strings.TrimSuffix(string(agent.Data["consoleURL"]), "/")
	if token == "" || consoleURL == "" {
		t.Fatalf("agent secret = %v", agent.Data)
	}

	e.passes(t, cpPool, 3)
	e.passes(t, "np6-workers", 2)
	all := e.f.CloudServers()
	if len(all) != 1 || all[0].Labels[hetzner.LabelBootstrap] != "true" || all[0].Labels[hetzner.LabelPool] != cpPool {
		t.Fatalf("servers = %+v", all)
	}
	ud := e.f.UserData(all[0].ID)
	if !strings.Contains(ud, "'--agent' '--console' '"+consoleURL+"' '--cluster-token' '"+token+"' '--platform' 'cloud' '--await-cloud-token'") || strings.Contains(ud, "kwft_join_") {
		t.Fatalf("user data:\n%s", ud)
	}
	p, _ := e.pass(t, "np6-workers")
	if _, msg := readyCondition(p); !strings.Contains(msg, JoinSecretName("np6")) {
		t.Fatalf("workers: %q", msg)
	}
}

func TestNodePoolRejectsBadLabels(t *testing.T) {
	e := newPoolEnv(t, "np7")
	createPool(t, "np7-pool", kwerftv1.NodePoolSpec{Cluster: "np7", ServerType: "cx23", Location: "fsn1", Count: 1,
		Labels: map[string]string{"node-role.kubernetes.io/master": "true"}})
	p, _ := e.pass(t, "np7-pool")
	if reason, _ := readyCondition(p); reason != "InvalidLabels" || len(e.f.CloudServers()) != 0 {
		t.Fatalf("reason %q", reason)
	}
}

func TestNodeRemoval(t *testing.T) {
	if k8s == nil {
		t.Skip("envtest not available")
	}
	ctx := context.Background()
	clock := &offsetClock{}
	r := &NodeRemovalReconciler{Client: k8s, APIReader: k8s, Now: clock.Now, EtcdRemoveTimeout: time.Hour}
	pass := func(name string) ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	annotate := func(name, key, value string) {
		t.Helper()
		var n corev1.Node
		if err := k8s.Get(ctx, client.ObjectKey{Name: name}, &n); err != nil {
			t.Fatal(err)
		}
		patch := client.MergeFrom(n.DeepCopy())
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[key] = value
		if err := k8s.Patch(ctx, &n, patch); err != nil {
			t.Fatal(err)
		}
	}
	status := func(name string) string {
		var n corev1.Node
		_ = k8s.Get(ctx, client.ObjectKey{Name: name}, &n)
		return n.Annotations[AnnotationDrainStatus]
	}

	addNode(t, "rm-cp", map[string]string{LabelControlPlane: "true"}, true)
	addNode(t, "rm-dedi", nil, true)
	addNode(t, "rm-pooled", map[string]string{hetzner.LabelPool: "some-pool"}, true)

	// A dedicated worker with an app pod covered by a PDB, and a DaemonSet pod.
	addPod(t, "rm-apps", "api-1", "rm-dedi", map[string]string{"app": "api"}, true)
	addPod(t, "rm-apps", "logs-agent", "rm-dedi", nil)
	var ds corev1.Pod
	_ = k8s.Get(ctx, client.ObjectKey{Namespace: "rm-apps", Name: "logs-agent"}, &ds)
	tr := true
	ds.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "logs", UID: "u1", Controller: &tr}}
	if err := k8s.Update(ctx, &ds); err != nil {
		t.Fatal(err)
	}
	minAvail := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "rm-apps"},
		Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvail, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}}}
	if err := k8s.Create(ctx, pdb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), pdb) })

	// Drain only: cordoned, the PDB holds the pod.
	annotate("rm-dedi", AnnotationNodeDrain, "true")
	pass("rm-dedi")
	var n corev1.Node
	_ = k8s.Get(ctx, client.ObjectKey{Name: "rm-dedi"}, &n)
	if !n.Spec.Unschedulable || !strings.Contains(status("rm-dedi"), "disruption budgets") || podGone(t, "rm-apps", "api-1") {
		t.Fatalf("unschedulable %v, status %q", n.Spec.Unschedulable, status("rm-dedi"))
	}
	// Past the drain timeout the pod goes anyway; the DaemonSet pod stays.
	clock.Advance(DefaultDrainTimeout + time.Minute)
	pass("rm-dedi")
	pass("rm-dedi")
	if !podGone(t, "rm-apps", "api-1") || podGone(t, "rm-apps", "logs-agent") || status("rm-dedi") != "Drained." || nodeGone("rm-dedi") {
		t.Fatalf("status %q", status("rm-dedi"))
	}

	// Remove: a node with a local volume waits unless forced.
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "rm-local-pv"}, Spec: corev1.PersistentVolumeSpec{
		Capacity:               corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/x"}},
		ClaimRef:               &corev1.ObjectReference{Namespace: "rm-apps", Name: "data"},
		NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"rm-dedi"}}}}}}},
	}}
	if err := k8s.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), pv) })
	annotate("rm-dedi", AnnotationNodeRemove, "true")
	pass("rm-dedi")
	if !strings.Contains(status("rm-dedi"), "rm-apps/data") || nodeGone("rm-dedi") {
		t.Fatalf("status %q", status("rm-dedi"))
	}
	annotate("rm-dedi", AnnotationNodeRemove, "force")
	pass("rm-dedi")
	if !nodeGone("rm-dedi") {
		t.Fatalf("not removed: %q", status("rm-dedi"))
	}

	// Pool nodes are the node pool reconciler's.
	annotate("rm-pooled", AnnotationNodeRemove, "true")
	pass("rm-pooled")
	if nodeGone("rm-pooled") || status("rm-pooled") != "" {
		t.Fatal("a pool node was handled here")
	}

	// The last control-plane node stays (fw-cp and other tests' nodes are
	// gone by now; only rm-cp is a control-plane node).
	annotate("rm-cp", AnnotationNodeRemove, "true")
	pass("rm-cp")
	if nodeGone("rm-cp") || !strings.Contains(status("rm-cp"), "last control-plane node") {
		t.Fatalf("status %q", status("rm-cp"))
	}
	// With a second, Ready one it leaves after its etcd member.
	addNode(t, "rm-cp2", map[string]string{LabelControlPlane: "true"}, true)
	pass("rm-cp")
	pass("rm-cp")
	_ = k8s.Get(ctx, client.ObjectKey{Name: "rm-cp"}, &n)
	if n.Annotations[annotationEtcdRemove] != "true" || nodeGone("rm-cp") {
		t.Fatalf("annotations %v", n.Annotations)
	}
	annotate("rm-cp", annotationEtcdRemoved, "rm-cp")
	pass("rm-cp")
	if !nodeGone("rm-cp") {
		t.Fatal("control-plane node not removed")
	}
}

func TestRenderBuildJobOnBuildNodes(t *testing.T) {
	run := testRun("dockerfile", "none")
	if pod := renderJob(t, run).Spec.Template.Spec; len(pod.Tolerations) != 0 || pod.Affinity != nil {
		t.Fatalf("no build pool, yet %+v %+v", pod.Tolerations, pod.Affinity)
	}
	run.buildNodes = true
	pod := renderJob(t, run).Spec.Template.Spec
	if len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != LabelBuildNode || pod.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("tolerations = %+v", pod.Tolerations)
	}
	terms := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || terms[0].MatchExpressions[0].Key != LabelBuildNode {
		t.Fatalf("affinity = %+v", pod.Affinity)
	}
}

func TestJoinScriptQuoting(t *testing.T) {
	j := joinScript{Console: "https://ops.example.com", Mode: "join", Token: "kwft_join_a.b", Role: "worker",
		Labels: map[string]string{"x": "it's"}, Cluster: "c", Pool: "p"}
	ud := j.userData()
	if !strings.Contains(ud, `'--node-label' 'x=it'"'"'s'`) || !strings.HasPrefix(ud, "#cloud-config\n") || !strings.Contains(ud, "runcmd:") {
		t.Fatalf("user data:\n%s", ud)
	}
	if len(ud) > hetzner.MaxUserData {
		t.Fatal("user data too large")
	}
	// The script in write_files parses.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	var script []string
	in := false
	for _, line := range strings.Split(ud, "\n") {
		switch {
		case strings.HasPrefix(line, "    content: |"):
			in = true
		case in && strings.HasPrefix(line, "      "):
			script = append(script, strings.TrimPrefix(line, "      "))
		case in && line == "":
			script = append(script, "")
		case in:
			in = false
		}
	}
	if out, err := exec.Command("bash", "-n", "-c", strings.Join(script, "\n")).CombinedOutput(); err != nil || len(script) < 10 {
		t.Fatalf("script does not parse: %v\n%s\n%s", err, out, strings.Join(script, "\n"))
	}
}
