package controllers

import (
	"context"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The SUC Plans of a Kubernetes upgrade (docs/phase6-upgrades.md ›
// Kubernetes upgrade), in namespace system-upgrade, owned by the Upgrade:
//
//   - k3s-server: control-plane nodes, one at a time, cordoned.
//   - k3s-agent: workers, one at a time, drained (evictions respect
//     PodDisruptionBudgets; 10 min timeout), after k3s-server is complete
//     (the k3s-upgrade image's "prepare k3s-server").
//   - k3s-agent-cordon: workers that are alone in their pool are only
//     cordoned: draining them would evict pods with nowhere to go, and a
//     k3s restart leaves containers running. Also after k3s-server.
//
// Workers of no node pool (joined by hand) count as one pool. The Plans
// use spec.version, so SUC runs rancher/k3s-upgrade:<version with "-">.

const (
	// Job deadlines: a server is cordoned and restarted; a worker may
	// first take the drain timeout.
	serverJobDeadline = 15 * 60
	agentJobDeadline  = 30 * 60
	drainTimeout      = "10m"
	// sucJobNameLabel is the label the Job controller puts on a Job's pods.
	sucJobNameLabel = "batch.kubernetes.io/job-name"
)

func isControlPlane(n *corev1.Node) bool { return n.Labels[upgrades.LabelControlPlane] == "true" }

func hostnameOf(n *corev1.Node) string {
	if h := n.Labels[corev1.LabelHostname]; h != "" {
		return h
	}
	return n.Name
}

// aloneInPool are the workers that are the only node of their pool.
func aloneInPool(nodes []corev1.Node) []string {
	pools := map[string][]string{}
	for i := range nodes {
		if !isControlPlane(&nodes[i]) {
			pool := nodes[i].Labels[hetzner.LabelPool]
			pools[pool] = append(pools[pool], hostnameOf(&nodes[i]))
		}
	}
	var out []string
	for _, hosts := range pools {
		if len(hosts) == 1 {
			out = append(out, hosts[0])
		}
	}
	slices.Sort(out)
	return out
}

func anys(ss ...string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func requirement(key, op string, values ...string) map[string]any {
	r := map[string]any{"key": key, "operator": op}
	if len(values) > 0 {
		r["values"] = anys(values...)
	}
	return r
}

// renderPlans are the Plans for u on these nodes.
func renderPlans(u *kwerftv1.Upgrade, nodes []corev1.Node) []*unstructured.Unstructured {
	plan := func(name string, spec map[string]any) *unstructured.Unstructured {
		p := &unstructured.Unstructured{}
		p.SetGroupVersionKind(upgrades.PlanGVK)
		p.SetNamespace(upgrades.SUCNamespace)
		p.SetName(name)
		p.SetLabels(map[string]string{upgrades.LabelUpgrade: u.Name, LabelManagedBy: ManagedByKwerft})
		p.SetOwnerReferences([]metav1.OwnerReference{upgrades.UpgradeOwner(u)})
		spec["concurrency"] = int64(1)
		spec["serviceAccountName"] = upgrades.SUCServiceAccount
		spec["version"] = u.Spec.Version
		spec["upgrade"] = map[string]any{"image": upgrades.K3sUpgradeImage}
		// The upgrade must run on every node, whatever its taints.
		spec["tolerations"] = []any{map[string]any{"operator": "Exists"}}
		p.Object["spec"] = spec
		return p
	}
	afterServers := map[string]any{"image": upgrades.K3sUpgradeImage, "args": anys("prepare", upgrades.PlanServer)}
	out := []*unstructured.Unstructured{plan(upgrades.PlanServer, map[string]any{
		"cordon":                true,
		"jobActiveDeadlineSecs": int64(serverJobDeadline),
		"nodeSelector":          map[string]any{"matchExpressions": []any{requirement(upgrades.LabelControlPlane, "In", "true")}},
	})}
	workers := 0
	for i := range nodes {
		if !isControlPlane(&nodes[i]) {
			workers++
		}
	}
	alone := aloneInPool(nodes)
	if workers > len(alone) {
		match := []any{requirement(upgrades.LabelControlPlane, "DoesNotExist")}
		if len(alone) > 0 {
			match = append(match, requirement(corev1.LabelHostname, "NotIn", alone...))
		}
		out = append(out, plan(upgrades.PlanAgent, map[string]any{
			"prepare":               afterServers,
			"jobActiveDeadlineSecs": int64(agentJobDeadline),
			"drain": map[string]any{
				"timeout":            drainTimeout,
				"ignoreDaemonSets":   true,
				"deleteEmptydirData": true,
				// Pods without a controller would block the drain; Kwerft
				// runs none of its own.
				"force": true,
			},
			"nodeSelector": map[string]any{"matchExpressions": match},
		}))
	}
	if len(alone) > 0 {
		out = append(out, plan(upgrades.PlanAgentCordon, map[string]any{
			"prepare":               afterServers,
			"cordon":                true,
			"jobActiveDeadlineSecs": int64(serverJobDeadline),
			"nodeSelector": map[string]any{"matchExpressions": []any{
				requirement(upgrades.LabelControlPlane, "DoesNotExist"),
				requirement(corev1.LabelHostname, "In", alone...),
			}},
		}))
	}
	return out
}

// ensurePlans creates u's Plans that are missing. A Plan of the same name
// left by another Upgrade is deleted first (done false: come back).
// Existing Plans of u are left as they are: changing one would make SUC
// apply it again.
func (k *KubernetesUpgrader) ensurePlans(ctx context.Context, u *kwerftv1.Upgrade, nodes []corev1.Node) (bool, error) {
	done := true
	for _, want := range renderPlans(u, nodes) {
		have := &unstructured.Unstructured{}
		have.SetGroupVersionKind(upgrades.PlanGVK)
		err := k.Client.Get(ctx, client.ObjectKeyFromObject(want), have)
		switch {
		case apierrors.IsNotFound(err):
			if err := k.Client.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
				return false, err
			}
			log.FromContext(ctx).Info("plan created", "upgrade", u.Name, "plan", want.GetName())
		case err != nil:
			return false, err
		case have.GetLabels()[upgrades.LabelUpgrade] != u.Name:
			if err := client.IgnoreNotFound(k.Client.Delete(ctx, have)); err != nil {
				return false, err
			}
			done = false
		}
	}
	return done, nil
}

// deletePlans removes u's Plans: SUC then deletes their Jobs and starts no
// further node.
func (k *KubernetesUpgrader) deletePlans(ctx context.Context, u *kwerftv1.Upgrade) error {
	for _, name := range upgrades.Plans {
		p := &unstructured.Unstructured{}
		p.SetGroupVersionKind(upgrades.PlanGVK)
		if err := k.Client.Get(ctx, client.ObjectKey{Namespace: upgrades.SUCNamespace, Name: name}, p); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return err
		}
		if p.GetLabels()[upgrades.LabelUpgrade] != u.Name {
			continue
		}
		if err := client.IgnoreNotFound(k.Client.Delete(ctx, p, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
			return err
		}
	}
	return nil
}

// nodeProgress is status.nodes: control-plane nodes first, then by name.
// A node is Done on the target version, Ready and with no Job of the
// Plans still running; a failed Job fails it; a running Job says what it
// does (waiting for the control plane, draining, upgrading).
func nodeProgress(nodes []corev1.Node, target upgrades.Version, jobs []batchv1.Job, pods []corev1.Pod) []kwerftv1.UpgradeNode {
	sorted := slices.Clone(nodes)
	slices.SortFunc(sorted, func(a, b corev1.Node) int {
		if ca, cb := isControlPlane(&a), isControlPlane(&b); ca != cb {
			if ca {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	out := make([]kwerftv1.UpgradeNode, 0, len(sorted))
	for i := range sorted {
		n := &sorted[i]
		st := kwerftv1.UpgradeNode{Name: n.Name, Version: n.Status.NodeInfo.KubeletVersion, State: NodeWaiting}
		v, err := upgrades.ParseVersion(st.Version)
		onTarget := err == nil && v.Compare(target) == 0
		job := latestJob(jobs, n.Name)
		switch {
		case job != nil && jobFailed(job):
			st.State = NodeFailed
			st.Message = jobFailedMessage(job)
		case job != nil && !jobComplete(job):
			st.State, st.Message = podStage(job, pods, onTarget)
		case job != nil && !onTarget:
			st.State, st.Message = NodeUpgrading, "waiting for the kubelet to report "+target.String()
		case onTarget && !NodeReady(n):
			st.State, st.Message = NodeUpgrading, "waiting for the node to be ready"
		case onTarget:
			st.State = NodeDone
		case isControlPlane(n):
			st.Message = "waiting for its turn"
		default:
			st.Message = "after the control plane"
		}
		out = append(out, st)
	}
	return out
}

func latestJob(jobs []batchv1.Job, node string) *batchv1.Job {
	var out *batchv1.Job
	for i := range jobs {
		j := &jobs[i]
		if j.Labels[upgrades.SUCLabelNode] != node {
			continue
		}
		if out == nil || out.CreationTimestamp.Before(&j.CreationTimestamp) ||
			(out.CreationTimestamp.Equal(&j.CreationTimestamp) && out.Name < j.Name) {
			out = j
		}
	}
	return out
}

func jobFailed(j *batchv1.Job) bool   { return jobCondition(j, batchv1.JobFailed) != nil }
func jobComplete(j *batchv1.Job) bool { return jobCondition(j, batchv1.JobComplete) != nil }

func jobFailedMessage(j *batchv1.Job) string {
	c := jobCondition(j, batchv1.JobFailed)
	msg := "Job " + upgrades.SUCNamespace + "/" + j.Name + " failed"
	if c.Reason == batchv1.JobReasonDeadlineExceeded {
		msg += " (it ran longer than its deadline: a drain blocked by a PodDisruptionBudget?)"
	} else if c.Message != "" {
		msg += ": " + c.Message
	}
	return msg
}

// podStage reads the running Job's pod: SUC's init containers are
// "prepare" (waiting for k3s-server), "drain" or "cordon", then the
// "upgrade" container.
func podStage(job *batchv1.Job, pods []corev1.Pod, onTarget bool) (string, string) {
	if onTarget {
		return NodeUpgrading, "restarting on the new version"
	}
	var pod *corev1.Pod
	for i := range pods {
		if pods[i].Labels[sucJobNameLabel] == job.Name || pods[i].Labels["job-name"] == job.Name {
			if pod == nil || pod.CreationTimestamp.Before(&pods[i].CreationTimestamp) {
				pod = &pods[i]
			}
		}
	}
	if pod == nil {
		return NodeUpgrading, "starting"
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
			continue
		}
		switch cs.Name {
		case "prepare":
			return NodeWaiting, "waiting for the control plane"
		case "drain":
			return NodeDraining, "evicting pods, respecting PodDisruptionBudgets"
		case "cordon":
			return NodeUpgrading, "cordoning"
		}
		break
	}
	return NodeUpgrading, "installing k3s"
}
