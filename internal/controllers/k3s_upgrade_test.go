package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// Kubernetes upgrades against the test API server with SUC's Plan CRD
// (testdata/crds, v0.20.2): the Upgrade reconciler with the
// KubernetesUpgrader, called pass by pass. The tests play the runner (the
// snapshot), SUC (Jobs, pods) and the kubelets (versions).

const (
	k3sFrom   = "v1.37.1+k3s1"
	k3sTarget = "v1.37.2+k3s1"
)

type fakeAPIServer struct {
	etcdErr error
	apis    []upgrades.DeprecatedAPI
	err     error
}

func (f *fakeAPIServer) EtcdReady(context.Context) error { return f.etcdErr }
func (f *fakeAPIServer) DeprecatedAPIs(context.Context) ([]upgrades.DeprecatedAPI, error) {
	return f.apis, f.err
}

type k3sEnv struct {
	*upgradeEnv
	k *KubernetesUpgrader
}

// The cluster: a server (installer node), two workers in pool "workers"
// and one alone in pool "solo".
var k3sNodes = []struct {
	name, pool string
	server     bool
}{{"k3s-server-1", "", true}, {"k3s-worker-1", "workers", false}, {"k3s-worker-2", "workers", false}, {"k3s-solo-1", "solo", false}}

func newK3sEnv(t *testing.T) *k3sEnv {
	t.Helper()
	e := &k3sEnv{upgradeEnv: newUpgradeEnv(t)}
	ctx := context.Background()
	checks := &UpgradeChecks{Reader: k8s, Releases: testReleases(), Registry: fakeOCI{}, Version: "0.5.0", APIServer: &fakeAPIServer{}}
	e.k = &KubernetesUpgrader{Client: k8s, Checks: checks, Runner: e.r, Namespace: GatewayNamespace, Now: e.clock.Now}
	e.r.Kubernetes = e.k

	for _, ns := range []string{upgrades.SUCNamespace, traefikNamespace} {
		if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	for _, n := range k3sNodes {
		labels := map[string]string{corev1.LabelHostname: n.name}
		if n.server {
			labels[upgrades.LabelControlPlane] = "true"
			labels[upgrades.LabelEtcd] = "true"
			labels[kwerftv1.LabelInstaller] = "true"
		}
		if n.pool != "" {
			labels[hetzner.LabelPool] = n.pool
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.name, Labels: labels}}
		mustCreate(t, node)
		setKubelet(t, n.name, k3sFrom, true)
	}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "c"}}}}
	for _, d := range []*appsv1.Deployment{
		{ObjectMeta: metav1.ObjectMeta{Namespace: upgrades.SUCNamespace, Name: upgrades.SUCDeployment}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: corednsNamespace, Name: corednsName}},
	} {
		d.Spec = appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Selector: selector, Template: template}
		mustCreate(t, d)
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1}
		if err := k8s.Status().Update(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, ds := range []*appsv1.DaemonSet{
		{ObjectMeta: metav1.ObjectMeta{Namespace: ciliumDSNamespace, Name: ciliumDaemonSet}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: traefikNamespace, Name: traefikName}},
	} {
		ds.Spec = appsv1.DaemonSetSpec{Selector: selector, Template: template}
		mustCreate(t, ds)
		setDaemonSet(t, ds.Namespace, ds.Name, 4)
	}
	acceptConsoleRoute(t)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, n := range k3sNodes {
			_ = k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.name}})
		}
		_ = k8s.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(upgrades.SUCNamespace), client.PropagationPolicy(metav1.DeletePropagationBackground))
		_ = k8s.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(upgrades.SUCNamespace), client.GracePeriodSeconds(0))
		for _, name := range upgrades.Plans {
			_ = k8s.Delete(ctx, sucPlan(name))
		}
	})
	return e
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8s.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), obj) })
}

func setKubelet(t *testing.T, name, version string, ready bool) {
	t.Helper()
	var n corev1.Node
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	n.Status.NodeInfo.KubeletVersion = version
	if err := k8s.Status().Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
}

func setDaemonSet(t *testing.T, ns, name string, available int32) {
	t.Helper()
	var ds appsv1.DaemonSet
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ds); err != nil {
		t.Fatal(err)
	}
	ds.Status = appsv1.DaemonSetStatus{ObservedGeneration: ds.Generation, DesiredNumberScheduled: 4, CurrentNumberScheduled: 4,
		UpdatedNumberScheduled: 4, NumberAvailable: available, NumberReady: available}
	if err := k8s.Status().Update(context.Background(), &ds); err != nil {
		t.Fatal(err)
	}
}

// acceptConsoleRoute plays Traefik for the console's route, which the
// Domain reconciler of the shared manager may have rendered.
func acceptConsoleRoute(t *testing.T) {
	t.Helper()
	var route gwv1.HTTPRoute
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: GatewayNamespace, Name: consoleRoute}, &route); err != nil {
		return
	}
	route.Status.Parents = []gwv1.RouteParentStatus{{ParentRef: gwv1.ParentReference{Name: "kwerft"}, ControllerName: "traefik.io/gateway-controller",
		Conditions: []metav1.Condition{{Type: string(gwv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: metav1.Now()}}}}
	if err := k8s.Status().Update(context.Background(), &route); err != nil {
		t.Fatal(err)
	}
}

func sucPlan(name string) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(upgrades.PlanGVK)
	p.SetNamespace(upgrades.SUCNamespace)
	p.SetName(name)
	return p
}

func getPlan(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	p := sucPlan(name)
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(p), p); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return p
}

// sucJob plays SUC: the Job applying a Plan on a node, with its pod in the
// given init container (empty: the upgrade container).
func sucJob(t *testing.T, planName, node, initContainer string) *batchv1.Job {
	t.Helper()
	labels := map[string]string{upgrades.SUCLabelPlan: planName, upgrades.SUCLabelNode: node}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: upgrades.SUCNamespace, Name: "apply-" + planName + "-on-" + node, Labels: labels},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "upgrade", Image: "rancher/k3s-upgrade"}}}}}}
	if err := k8s.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	podLabels := map[string]string{upgrades.SUCLabelPlan: planName, sucJobNameLabel: job.Name}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: upgrades.SUCNamespace, Name: job.Name + "-pod", Labels: podLabels},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{{Name: "prepare", Image: "k"}, {Name: "drain", Image: "k"}},
			Containers:     []corev1.Container{{Name: "upgrade", Image: "rancher/k3s-upgrade"}}}}
	if err := k8s.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "prepare", State: done}, {Name: "drain", State: done}}
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == initContainer {
			pod.Status.InitContainerStatuses[i].State = running
		}
	}
	if err := k8s.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return job
}

func failJob(t *testing.T, job *batchv1.Job) {
	t.Helper()
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded, LastTransitionTime: now},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded, Message: "Job has reached the specified backoff limit", LastTransitionTime: now},
	}
	if err := k8s.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func removeJob(t *testing.T, job *batchv1.Job) {
	t.Helper()
	if err := k8s.Delete(context.Background(), job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatal(err)
	}
	_ = k8s.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: upgrades.SUCNamespace, Name: job.Name + "-pod"}}, client.GracePeriodSeconds(0))
}

// toRunning starts a Kubernetes upgrade and plays the runner's snapshot.
func (e *k3sEnv) toRunning(t *testing.T, name string) *kwerftv1.Upgrade {
	t.Helper()
	createUpgrade(t, name, k3sTarget, map[string]string{kwerftv1.AnnotationRequestedBy: "alice@example.com"})
	u, _ := e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeBackingUp {
		t.Fatalf("after preflight: %s %q %+v", u.Status.Phase, u.Status.Message, Blocked(u.Status.Preflight))
	}
	u.Status.Backup = &kwerftv1.UpgradeBackup{EtcdSnapshot: upgrades.SnapshotName(name)}
	if err := k8s.Status().Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	u, _ = e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeRunning {
		t.Fatalf("after the snapshot: %s %q", u.Status.Phase, u.Status.Message)
	}
	return u
}

func nodeStates(u *kwerftv1.Upgrade) string {
	var out []string
	for _, n := range u.Status.Nodes {
		out = append(out, n.Name+"="+n.State)
	}
	return strings.Join(out, " ")
}

func TestKubernetesUpgradeNodeByNode(t *testing.T) {
	e := newK3sEnv(t)
	ctx := context.Background()
	const name = "kubernetes-v1.37.2-k3s1-a"
	createUpgrade(t, name, k3sTarget, map[string]string{kwerftv1.AnnotationRequestedBy: "alice@example.com"})
	u, _ := e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeBackingUp {
		t.Fatalf("after preflight: %s %q", u.Status.Phase, u.Status.Message)
	}
	if b := Blocked(u.Status.Preflight); len(b) > 0 {
		t.Fatalf("blocked: %+v", b)
	}
	if u.Status.From == nil || u.Status.From.Kubernetes != k3sFrom {
		t.Errorf("from = %+v", u.Status.From)
	}
	// The runner takes the snapshot: its Job, no finalizer.
	job := runnerJob(t, name)
	if job == nil || job.Spec.Template.Spec.Containers[0].Image != testRunnerImage || slices.Contains(u.Finalizers, upgradeFinalizer) {
		t.Fatalf("runner job = %v, finalizers %v", job, u.Finalizers)
	}
	var cm corev1.ConfigMap
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: upgrades.LogConfigMapName(name)}, &cm); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(cm.Data[AppsBaselineKey])) || !strings.Contains(cm.Data[upgrades.LogKey], "requested by alice@example.com") {
		t.Errorf("log ConfigMap = %v", cm.Data)
	}
	// Without the snapshot it stays in Backup.
	if u, _ = e.settle(t, name); u.Status.Phase != kwerftv1.UpgradeBackingUp {
		t.Fatalf("no snapshot yet: %s", u.Status.Phase)
	}
	u.Status.Backup = &kwerftv1.UpgradeBackup{EtcdSnapshot: upgrades.SnapshotName(name)}
	if err := k8s.Status().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	u, _ = e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeRunning {
		t.Fatalf("after the snapshot: %s %q", u.Status.Phase, u.Status.Message)
	}
	if got := nodeStates(u); got != "k3s-server-1=Waiting k3s-solo-1=Waiting k3s-worker-1=Waiting k3s-worker-2=Waiting" {
		t.Errorf("nodes = %s", got)
	}

	// The Plans, valid for SUC's CRD.
	server, agent, cordon := getPlan(t, upgrades.PlanServer), getPlan(t, upgrades.PlanAgent), getPlan(t, upgrades.PlanAgentCordon)
	if server == nil || agent == nil || cordon == nil {
		t.Fatalf("plans: %v %v %v", server, agent, cordon)
	}
	for _, p := range []*unstructured.Unstructured{server, agent, cordon} {
		version, _, _ := unstructured.NestedString(p.Object, "spec", "version")
		conc, _, _ := unstructured.NestedInt64(p.Object, "spec", "concurrency")
		image, _, _ := unstructured.NestedString(p.Object, "spec", "upgrade", "image")
		sa, _, _ := unstructured.NestedString(p.Object, "spec", "serviceAccountName")
		if version != k3sTarget || conc != 1 || image != upgrades.K3sUpgradeImage || sa != upgrades.SUCServiceAccount ||
			p.GetLabels()[upgrades.LabelUpgrade] != name || len(p.GetOwnerReferences()) != 1 || p.GetOwnerReferences()[0].UID != u.UID {
			t.Errorf("plan %s: version %s concurrency %d image %s sa %s labels %v", p.GetName(), version, conc, image, sa, p.GetLabels())
		}
	}
	if c, _, _ := unstructured.NestedBool(server.Object, "spec", "cordon"); !c {
		t.Error("the server plan does not cordon")
	}
	if _, has, _ := unstructured.NestedMap(server.Object, "spec", "drain"); has {
		t.Error("the server plan drains")
	}
	timeout, _, _ := unstructured.NestedString(agent.Object, "spec", "drain", "timeout")
	evict, _, _ := unstructured.NestedBool(agent.Object, "spec", "drain", "disableEviction")
	prepare, _, _ := unstructured.NestedStringSlice(agent.Object, "spec", "prepare", "args")
	if timeout != "10m" || evict || !slices.Equal(prepare, []string{"prepare", upgrades.PlanServer}) {
		t.Errorf("agent plan: drain timeout %q, disableEviction %v, prepare %v", timeout, evict, prepare)
	}
	agentSel, _, _ := unstructured.NestedSlice(agent.Object, "spec", "nodeSelector", "matchExpressions")
	cordonSel, _, _ := unstructured.NestedSlice(cordon.Object, "spec", "nodeSelector", "matchExpressions")
	if s := fmt.Sprint(agentSel); !strings.Contains(s, "DoesNotExist") || !strings.Contains(s, "NotIn") || !strings.Contains(s, "k3s-solo-1") {
		t.Errorf("agent selector %s", s)
	}
	if s := fmt.Sprint(cordonSel); !strings.Contains(s, "operator:In") || !strings.Contains(s, "k3s-solo-1") {
		t.Errorf("cordon selector %s", s)
	}
	if _, has, _ := unstructured.NestedMap(cordon.Object, "spec", "drain"); has {
		t.Error("the single node is drained")
	}

	// SUC upgrades the server.
	sj := sucJob(t, upgrades.PlanServer, "k3s-server-1", "")
	u, _ = e.settle(t, name)
	if u.Status.Nodes[0].State != NodeUpgrading || !strings.Contains(u.Status.Message, "0 of 4 done") {
		t.Errorf("server upgrading: %s %q", nodeStates(u), u.Status.Message)
	}
	setKubelet(t, "k3s-server-1", k3sTarget, true)
	removeJob(t, sj)
	// A worker drains, the other waits for its turn.
	wj := sucJob(t, upgrades.PlanAgent, "k3s-worker-1", "drain")
	u, _ = e.settle(t, name)
	if got := nodeStates(u); got != "k3s-server-1=Done k3s-solo-1=Waiting k3s-worker-1=Draining k3s-worker-2=Waiting" {
		t.Errorf("nodes = %s", got)
	}
	if u.Status.Nodes[0].Version != k3sTarget {
		t.Errorf("server version %s", u.Status.Nodes[0].Version)
	}
	for _, n := range []string{"k3s-worker-1", "k3s-worker-2", "k3s-solo-1"} {
		setKubelet(t, n, k3sTarget, true)
	}
	removeJob(t, wj)
	u, _ = e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeSucceeded || u.Status.FinishedAt == nil {
		t.Fatalf("after the nodes: %s %q", u.Status.Phase, u.Status.Message)
	}
	if !meta.IsStatusConditionTrue(u.Status.Conditions, ConditionNodesUpgraded) || !meta.IsStatusConditionTrue(u.Status.Conditions, ConditionVerified) {
		t.Errorf("conditions = %+v", u.Status.Conditions)
	}
	for _, p := range upgrades.Plans {
		if getPlan(t, p) != nil {
			t.Errorf("plan %s left after success", p)
		}
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: upgrades.LogConfigMapName(name)}, &cm); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"k3s-worker-1: Draining", "Every node runs " + k3sTarget, "runs on all 4 nodes"} {
		if !strings.Contains(cm.Data[upgrades.LogKey], want) {
			t.Errorf("log lacks %q:\n%s", want, cm.Data[upgrades.LogKey])
		}
	}
}

func TestKubernetesUpgradeFailureStopsTheNodes(t *testing.T) {
	e := newK3sEnv(t)
	const name = "kubernetes-v1.37.2-k3s1-b"
	e.toRunning(t, name)
	setKubelet(t, "k3s-server-1", k3sTarget, true)
	job := sucJob(t, upgrades.PlanAgent, "k3s-worker-1", "drain")
	failJob(t, job)
	u, _ := e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Kubernetes" {
		t.Fatalf("status = %s %s %q", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
	for _, want := range []string{"on node k3s-worker-1", "backoff limit", "no further node is upgraded", "On " + k3sTarget + ": k3s-server-1",
		"Still on " + k3sFrom + ": k3s-solo-1, k3s-worker-1, k3s-worker-2", upgrades.SnapshotName(name), "--cluster-reset"} {
		if !strings.Contains(u.Status.Message, want) {
			t.Errorf("message lacks %q: %s", want, u.Status.Message)
		}
	}
	if got := nodeStates(u); got != "k3s-server-1=Done k3s-solo-1=Waiting k3s-worker-1=Failed k3s-worker-2=Waiting" {
		t.Errorf("nodes = %s", got)
	}
	for _, p := range upgrades.Plans {
		if getPlan(t, p) != nil {
			t.Errorf("plan %s left after the failure", p)
		}
	}
}

func TestKubernetesUpgradeVerifyTimesOut(t *testing.T) {
	e := newK3sEnv(t)
	const name = "kubernetes-v1.37.2-k3s1-c"
	e.toRunning(t, name)
	for _, n := range k3sNodes {
		setKubelet(t, n.name, k3sTarget, true)
	}
	setDaemonSet(t, ciliumDSNamespace, ciliumDaemonSet, 3)
	u, _ := e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeVerifying || !strings.Contains(u.Status.Message, "Cilium is not ready (3 of 4 available)") {
		t.Fatalf("status = %s %q", u.Status.Phase, u.Status.Message)
	}
	e.clock.Advance(11 * time.Minute)
	u, _ = e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Verify" || !strings.Contains(u.Status.Message, "verification failed: Cilium") ||
		!strings.Contains(u.Status.Message, upgrades.SnapshotName(name)) {
		t.Errorf("status = %s %s %q", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
}

func TestKubernetesUpgradeCancelInBackup(t *testing.T) {
	e := newK3sEnv(t)
	const name = "kubernetes-v1.37.2-k3s1-d"
	createUpgrade(t, name, k3sTarget, nil)
	e.settle(t, name)
	if runnerJob(t, name) == nil {
		t.Fatal("no runner Job")
	}
	u := getUpgrade(t, name)
	u.Annotations = map[string]string{kwerftv1.AnnotationCancelRequested: "bob@example.com"}
	if err := k8s.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	u, _ = e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeCancelled || !strings.Contains(u.Status.Message, "bob@example.com") {
		t.Errorf("status = %s %q", u.Status.Phase, u.Status.Message)
	}
	if job := runnerJob(t, name); job != nil && job.DeletionTimestamp.IsZero() {
		t.Error("the runner Job stays")
	}
	if getPlan(t, upgrades.PlanServer) != nil {
		t.Error("a plan after a cancel")
	}
}

func TestKubernetesUpgradePreflightBlocksAndStartsNothing(t *testing.T) {
	e := newK3sEnv(t)
	const name = "kubernetes-v1.39.0-k3s1-e"
	createUpgrade(t, name, "v1.39.0+k3s1", nil) // two minors ahead
	u, _ := e.settle(t, name)
	if u.Status.Phase != kwerftv1.UpgradeFailed || u.Status.Reason != "Preflight" || !strings.Contains(u.Status.Message, "one minor release at a time") {
		t.Fatalf("status = %s %s %q", u.Status.Phase, u.Status.Reason, u.Status.Message)
	}
	if runnerJob(t, name) != nil || getPlan(t, upgrades.PlanServer) != nil {
		t.Error("a failed preflight started something")
	}
}
