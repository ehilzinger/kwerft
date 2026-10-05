package controllers

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// KubernetesUpgrader drives a Kubernetes (k3s) Upgrade from Preflight to
// its end (docs/phase6-upgrades.md › Kubernetes upgrade); the Upgrade
// controller calls it for the active one and writes the status.
//
//	Preflight ─► the k3s checks; the Apps' ready replicas as the baseline
//	Backup ─► the runner Job takes `k3s etcd-snapshot save` on the installer node
//	Running ─► SUC Plans k3s-server, then k3s-agent (drain) and k3s-agent-cordon
//	           (single-node pools), one node at a time; status.nodes per node
//	Verifying ─► nodes on the target and Ready, Cilium, CoreDNS, Traefik, Gateway
//	             API CRDs, the console's route, Apps back (VerifyTimeout)
//	Succeeded | Failed ─► Plans deleted
//
// There is no automatic rollback: k3s cannot be downgraded and restoring
// etcd resets the cluster. A failure deletes the Plans (no further node is
// touched) and names the node and the snapshot, with the restore procedure.
type KubernetesUpgrader struct {
	Client client.Client
	// Checks runs the preflight (UpgradeChecks.Kubernetes).
	Checks *UpgradeChecks
	// Runner starts the runner Job that takes the etcd snapshot (the
	// UpgradeReconciler).
	Runner KubernetesRunner
	// Namespace of the log ConfigMap (kwerft-system).
	Namespace string
	// VerifyTimeout bounds Verifying (default 10 min); NodeTimeout is how
	// long one node may take (default 40 min: a 10 min drain, the
	// download, k3s's restart).
	VerifyTimeout, NodeTimeout time.Duration
	Now                        func() time.Time
}

// KubernetesRunner is the part of the Upgrade controller the Kubernetes
// upgrade uses for its etcd snapshot.
type KubernetesRunner interface {
	EnsureRunner(ctx context.Context, u *kwerftv1.Upgrade) (*batchv1.Job, error)
	DeleteRunner(ctx context.Context, u *kwerftv1.Upgrade) error
}

// Node states in status.nodes.
const (
	NodeWaiting   = "Waiting"
	NodeDraining  = "Draining"
	NodeUpgrading = "Upgrading"
	NodeDone      = "Done"
	NodeFailed    = "Failed"
)

// Conditions on a Kubernetes Upgrade; their lastTransitionTime is when the
// nodes started and when verification started.
const (
	ConditionNodesUpgraded = "NodesUpgraded"
	ConditionVerified      = "Verified"
)

const (
	defaultK3sVerifyTimeout = 10 * time.Minute
	defaultK3sNodeTimeout   = 40 * time.Minute
	// AppsBaselineKey holds the Apps' ready replicas before a Kubernetes
	// upgrade in its log ConfigMap.
	AppsBaselineKey = "apps-before.json"
	k3sPoll         = 10 * time.Second
	// Cilium, CoreDNS and Traefik as the installer and k3s deploy them.
	ciliumDSNamespace, ciliumDaemonSet = "kube-system", "cilium"
	corednsNamespace, corednsName      = "kube-system", "coredns"
	traefikNamespace, traefikName      = "traefik", "traefik"
	gatewayAPIGroup                    = "gateway.networking.k8s.io"
)

// gatewayCRDs must be established after the upgrade (k3s ≥ 1.37 packages
// the Gateway API).
var gatewayCRDs = []string{"gatewayclasses", "gateways", "httproutes"}

func (k *KubernetesUpgrader) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *KubernetesUpgrader) ns() string { return cmp.Or(k.Namespace, GatewayNamespace) }

func (k *KubernetesUpgrader) Reconcile(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	switch u.Status.Phase {
	case kwerftv1.UpgradePreflight:
		return k.preflight(ctx, u)
	case kwerftv1.UpgradeBackingUp:
		return k.backup(ctx, u)
	case kwerftv1.UpgradeRunning:
		return k.running(ctx, u)
	case kwerftv1.UpgradeVerifying:
		return k.verifying(ctx, u)
	}
	// RollingBack is not a Kubernetes upgrade's phase.
	return ctrl.Result{}, k.failAfter(ctx, u, "Kubernetes", fmt.Sprintf("Unexpected phase %q.", u.Status.Phase), "")
}

func (k *KubernetesUpgrader) finish(u *kwerftv1.Upgrade, phase kwerftv1.UpgradePhase, reason, msg string) {
	u.Status.Phase = phase
	u.Status.Reason, u.Status.Message = reason, msg
	u.Status.FinishedAt = &metav1.Time{Time: k.now()}
}

func (k *KubernetesUpgrader) target(u *kwerftv1.Upgrade) upgrades.Version {
	v, _ := upgrades.ParseVersion(u.Spec.Version) // the preflight checked it
	return v
}

func (k *KubernetesUpgrader) preflight(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	if k.Checks == nil {
		k.finish(u, kwerftv1.UpgradeFailed, "Preflight", "Kubernetes upgrades are not available here.")
		return ctrl.Result{}, nil
	}
	checks := k.Checks.Kubernetes(ctx, u.Spec, u.Name)
	u.Status.Preflight = checks
	if blocked := Blocked(checks); len(blocked) > 0 {
		var msgs []string
		for _, c := range blocked {
			msgs = append(msgs, c.Check+": "+c.Message)
		}
		k.finish(u, kwerftv1.UpgradeFailed, "Preflight", "Preflight failed. "+strings.Join(msgs, " "))
		return ctrl.Result{}, nil
	}
	// The Apps' ready replicas now, to compare with after the upgrade.
	apps, err := (&upgrades.KubeCluster{Client: k.Client}).AppReplicas(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	raw, _ := json.Marshal(apps)
	if err := k.writeLog(ctx, u, map[string]string{AppsBaselineKey: string(raw)},
		fmt.Sprintf("Kubernetes upgrade %s → %s requested by %s.", fromKubernetes(u), u.Spec.Version, cmp.Or(u.Annotations[kwerftv1.AnnotationRequestedBy], "?"))); err != nil {
		return ctrl.Result{}, err
	}
	u.Status.Phase = kwerftv1.UpgradeBackingUp
	u.Status.Message = "Taking an etcd snapshot on the installer node."
	return ctrl.Result{Requeue: true}, nil
}

func fromKubernetes(u *kwerftv1.Upgrade) string {
	if u.Status.From != nil && u.Status.From.Kubernetes != "" {
		return u.Status.From.Kubernetes
	}
	return "?"
}

func snapshotOf(u *kwerftv1.Upgrade) string {
	if u.Status.Backup != nil {
		return u.Status.Backup.EtcdSnapshot
	}
	return ""
}

// backup: the runner takes the etcd snapshot; then the Plans start.
func (k *KubernetesUpgrader) backup(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	if by := u.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
		if err := k.Runner.DeleteRunner(ctx, u); err != nil {
			return ctrl.Result{}, err
		}
		k.finish(u, kwerftv1.UpgradeCancelled, "Cancelled", "Cancelled by "+by+" before anything changed.")
		return ctrl.Result{}, nil
	}
	if snapshotOf(u) != "" {
		return k.start(ctx, u)
	}
	job, err := k.Runner.EnsureRunner(ctx, u)
	if err != nil || upgrades.Finished(u.Status.Phase) {
		return ctrl.Result{}, err
	}
	if job != nil {
		if _, msg := jobFailure(job); msg != "" {
			k.finish(u, kwerftv1.UpgradeFailed, "Backup", "The etcd snapshot did not complete. "+msg+" Nothing was changed.")
			return ctrl.Result{}, nil
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// start creates the Plans and moves to Running.
func (k *KubernetesUpgrader) start(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	var nodes corev1.NodeList
	if err := k.Client.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}
	if done, err := k.ensurePlans(ctx, u, nodes.Items); err != nil || !done {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	now := k.now()
	u.Status.Phase = kwerftv1.UpgradeRunning
	u.Status.Message = "Upgrading the nodes one at a time, the control plane first."
	u.Status.Nodes = nodeProgress(nodes.Items, k.target(u), nil, nil)
	meta.SetStatusCondition(&u.Status.Conditions, metav1.Condition{Type: ConditionNodesUpgraded, Status: metav1.ConditionFalse,
		Reason: "Upgrading", Message: "The nodes are being upgraded.", LastTransitionTime: metav1.NewTime(now), ObservedGeneration: u.Generation})
	log.FromContext(ctx).Info("kubernetes upgrade: plans created", "upgrade", u.Name, "version", u.Spec.Version)
	if err := k.writeLog(ctx, u, nil, "etcd snapshot "+snapshotOf(u)+" taken; upgrading the nodes."); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: k3sPoll}, nil
}

// running follows the nodes through the Plans' Jobs.
func (k *KubernetesUpgrader) running(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	var nodes corev1.NodeList
	if err := k.Client.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}
	// Plans missing now (deleted by hand) are created again; nodes that
	// are done are not touched again by SUC (same version).
	if _, err := k.ensurePlans(ctx, u, nodes.Items); err != nil {
		return ctrl.Result{}, err
	}
	var jobs batchv1.JobList
	if err := k.Client.List(ctx, &jobs, client.InNamespace(upgrades.SUCNamespace), client.HasLabels{upgrades.SUCLabelPlan}); err != nil {
		return ctrl.Result{}, err
	}
	var pods corev1.PodList
	if err := k.Client.List(ctx, &pods, client.InNamespace(upgrades.SUCNamespace), client.HasLabels{upgrades.SUCLabelPlan}); err != nil {
		return ctrl.Result{}, err
	}
	ours := slices.DeleteFunc(jobs.Items, func(j batchv1.Job) bool { return !slices.Contains(upgrades.Plans, j.Labels[upgrades.SUCLabelPlan]) })
	progress := nodeProgress(nodes.Items, k.target(u), ours, pods.Items)
	if lines := progressLines(u.Status.Nodes, progress); len(lines) > 0 {
		if err := k.writeLog(ctx, u, nil, lines...); err != nil {
			return ctrl.Result{}, err
		}
	}
	u.Status.Nodes = progress

	for _, n := range progress {
		if n.State == NodeFailed {
			return ctrl.Result{}, k.failAfter(ctx, u, "Kubernetes", fmt.Sprintf("The upgrade failed on node %s: %s", n.Name, sentence(n.Message)), n.Name)
		}
	}
	if node, msg := k.planFailure(ctx); msg != "" {
		return ctrl.Result{}, k.failAfter(ctx, u, "Kubernetes", "system-upgrade-controller reports: "+sentence(msg), node)
	}
	var waiting []string
	for _, n := range progress {
		if n.State != NodeDone {
			waiting = append(waiting, n.Name)
		}
	}
	if len(waiting) == 0 {
		now := k.now()
		u.Status.Phase = kwerftv1.UpgradeVerifying
		u.Status.Message = "Verifying Kubernetes " + u.Spec.Version + "."
		meta.SetStatusCondition(&u.Status.Conditions, metav1.Condition{Type: ConditionNodesUpgraded, Status: metav1.ConditionTrue,
			Reason: "Upgraded", Message: fmt.Sprintf("All %d nodes run %s.", len(progress), u.Spec.Version), LastTransitionTime: metav1.NewTime(now), ObservedGeneration: u.Generation})
		meta.SetStatusCondition(&u.Status.Conditions, metav1.Condition{Type: ConditionVerified, Status: metav1.ConditionFalse,
			Reason: "Verifying", Message: "Verifying.", LastTransitionTime: metav1.NewTime(now), ObservedGeneration: u.Generation})
		return ctrl.Result{Requeue: true}, k.writeLog(ctx, u, nil, "Every node runs "+u.Spec.Version+"; verifying.")
	}
	started := k.now()
	if c := meta.FindStatusCondition(u.Status.Conditions, ConditionNodesUpgraded); c != nil {
		started = c.LastTransitionTime.Time
	}
	limit := time.Duration(len(progress))*cmp.Or(k.NodeTimeout, defaultK3sNodeTimeout) + 10*time.Minute
	if k.now().Sub(started) > limit {
		current := ""
		for _, n := range progress {
			if n.State == NodeDraining || n.State == NodeUpgrading {
				current = n.Name
			}
		}
		return ctrl.Result{}, k.failAfter(ctx, u, "Timeout", fmt.Sprintf("The nodes were not all upgraded within %s; not done: %s.", limit, strings.Join(waiting, ", ")), current)
	}
	done := len(progress) - len(waiting)
	u.Status.Message = fmt.Sprintf("Upgrading the nodes one at a time: %d of %d done.", done, len(progress))
	for _, n := range progress {
		if n.State == NodeDraining || n.State == NodeUpgrading {
			u.Status.Message += " " + n.Name + ": " + strings.ToLower(n.State)
			if n.Message != "" {
				u.Status.Message += " (" + n.Message + ")"
			}
			u.Status.Message += "."
		}
	}
	return ctrl.Result{RequeueAfter: k3sPoll}, nil
}

// planFailure reads SUC's verdict on its Plans: Complete=False with
// reason JobFailed names the Job and node (the Job itself may be gone
// after its TTL).
func (k *KubernetesUpgrader) planFailure(ctx context.Context) (node, msg string) {
	for _, name := range upgrades.Plans {
		p := &unstructured.Unstructured{}
		p.SetGroupVersionKind(upgrades.PlanGVK)
		if err := k.Client.Get(ctx, client.ObjectKey{Namespace: upgrades.SUCNamespace, Name: name}, p); err != nil {
			continue
		}
		conds, _, _ := unstructured.NestedSlice(p.Object, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] == "Complete" && m["status"] == "False" && m["reason"] == "JobFailed" {
				text, _ := m["message"].(string)
				if _, after, ok := strings.Cut(text, " on Node "); ok {
					node, _, _ = strings.Cut(after, ":")
				}
				return node, text
			}
		}
	}
	return "", ""
}

// verifying waits for the cluster to be whole again on the target.
func (k *KubernetesUpgrader) verifying(ctx context.Context, u *kwerftv1.Upgrade) (ctrl.Result, error) {
	problems, err := k.verify(ctx, u)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := k.now()
	if len(problems) == 0 {
		if err := k.deletePlans(ctx, u); err != nil {
			return ctrl.Result{}, err
		}
		k.finish(u, kwerftv1.UpgradeSucceeded, "", fmt.Sprintf("Kubernetes %s runs on all %d nodes.", u.Spec.Version, len(u.Status.Nodes)))
		meta.SetStatusCondition(&u.Status.Conditions, metav1.Condition{Type: ConditionVerified, Status: metav1.ConditionTrue,
			Reason: "Verified", Message: "Nodes, network, DNS, ingress and Apps are back.", LastTransitionTime: metav1.NewTime(now), ObservedGeneration: u.Generation})
		return ctrl.Result{}, k.writeLog(ctx, u, nil, u.Status.Message)
	}
	started := now
	if c := meta.FindStatusCondition(u.Status.Conditions, ConditionVerified); c != nil {
		started = c.LastTransitionTime.Time
	}
	if now.Sub(started) >= cmp.Or(k.VerifyTimeout, defaultK3sVerifyTimeout) {
		return ctrl.Result{}, k.failAfter(ctx, u, "Verify", "Every node was upgraded, but verification failed: "+strings.Join(problems, "; ")+".", "")
	}
	u.Status.Message = "Verifying: " + strings.Join(problems, "; ") + "."
	return ctrl.Result{RequeueAfter: k3sPoll}, nil
}

// verify lists what is not (yet) as it should be after the upgrade.
func (k *KubernetesUpgrader) verify(ctx context.Context, u *kwerftv1.Upgrade) ([]string, error) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	target := k.target(u)

	var nodes corev1.NodeList
	if err := k.Client.List(ctx, &nodes); err != nil {
		return nil, err
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		v, err := upgrades.ParseVersion(n.Status.NodeInfo.KubeletVersion)
		switch {
		case err != nil || v.Compare(target) != 0:
			add("node %s runs %s", n.Name, n.Status.NodeInfo.KubeletVersion)
		case !NodeReady(n):
			add("node %s is not ready", n.Name)
		case n.Spec.Unschedulable:
			add("node %s is still cordoned", n.Name)
		}
	}
	var cilium appsv1.DaemonSet
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: ciliumDSNamespace, Name: ciliumDaemonSet}, &cilium); err != nil {
		add("Cilium: %v", notFoundOr(err))
	} else if p := daemonSetProblem(&cilium); p != "" {
		add("Cilium %s", p)
	}
	var coredns appsv1.Deployment
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: corednsNamespace, Name: corednsName}, &coredns); err != nil {
		add("CoreDNS: %v", notFoundOr(err))
	} else if p := deploymentProblem(&coredns); p != "" {
		add("CoreDNS %s", p)
	}
	if p := k.traefikProblem(ctx); p != "" {
		add("%s", p)
	}
	for _, name := range gatewayCRDs {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		if err := k.Client.Get(ctx, client.ObjectKey{Name: name + "." + gatewayAPIGroup}, crd); err != nil {
			add("Gateway API CRD %s: %v", name, notFoundOr(err))
		} else if !crdEstablished(crd) {
			add("Gateway API CRD %s is not established", name)
		}
	}
	if p := k.consoleRouteProblem(ctx); p != "" {
		add("%s", p)
	}
	before, err := k.appsBaseline(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(before) > 0 {
		now, err := (&upgrades.KubeCluster{Client: k.Client}).AppReplicas(ctx)
		if err != nil {
			return nil, err
		}
		if p := upgrades.AppsBehind(before, now); p != "" {
			add("%s", p)
		}
	}
	return problems, nil
}

func notFoundOr(err error) string {
	if apierrors.IsNotFound(err) {
		return "missing"
	}
	return err.Error()
}

// traefikProblem: Traefik runs as a DaemonSet (install.sh); a Deployment
// is accepted too.
func (k *KubernetesUpgrader) traefikProblem(ctx context.Context) string {
	var ds appsv1.DaemonSet
	err := k.Client.Get(ctx, client.ObjectKey{Namespace: traefikNamespace, Name: traefikName}, &ds)
	if err == nil {
		if p := daemonSetProblem(&ds); p != "" {
			return "Traefik " + p
		}
		return ""
	}
	if !apierrors.IsNotFound(err) {
		return "Traefik: " + err.Error()
	}
	var d appsv1.Deployment
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: traefikNamespace, Name: traefikName}, &d); err != nil {
		return "Traefik: " + notFoundOr(err)
	}
	if p := deploymentProblem(&d); p != "" {
		return "Traefik " + p
	}
	return ""
}

// consoleRouteProblem: the console's HTTPS route is Accepted by every
// parent. Agent clusters and consoles without a domain have none.
func (k *KubernetesUpgrader) consoleRouteProblem(ctx context.Context) string {
	var route gwv1.HTTPRoute
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: consoleRoute}, &route); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return ""
		}
		return "the console's route: " + err.Error()
	}
	if len(route.Status.Parents) == 0 {
		return "the console's route is not accepted yet"
	}
	for _, p := range route.Status.Parents {
		if !meta.IsStatusConditionTrue(p.Conditions, string(gwv1.RouteConditionAccepted)) {
			return "the console's route is not accepted"
		}
	}
	return ""
}

func (k *KubernetesUpgrader) appsBaseline(ctx context.Context, u *kwerftv1.Upgrade) (map[string]int32, error) {
	var cm corev1.ConfigMap
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.ns(), Name: upgrades.LogConfigMapName(u.Name)}, &cm); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	apps := map[string]int32{}
	if raw := cm.Data[AppsBaselineKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &apps); err != nil {
			return nil, nil // unreadable: nothing to compare with
		}
	}
	return apps, nil
}

// failAfter ends an Upgrade that changed nodes: the Plans go (no further
// node is touched), the node is marked, the message names the snapshot and
// how to restore it.
func (k *KubernetesUpgrader) failAfter(ctx context.Context, u *kwerftv1.Upgrade, reason, cause, node string) error {
	if err := k.deletePlans(ctx, u); err != nil {
		return err
	}
	target := k.target(u)
	var upgraded, behind []string
	for i := range u.Status.Nodes {
		n := &u.Status.Nodes[i]
		if n.Name == node && n.State != NodeDone {
			n.State = NodeFailed
		}
		if v, err := upgrades.ParseVersion(n.Version); err == nil && v.Compare(target) == 0 {
			upgraded = append(upgraded, n.Name)
		} else {
			behind = append(behind, n.Name)
		}
	}
	msg := sentence(cause) + " The upgrade plans were deleted, so no further node is upgraded."
	if len(upgraded) > 0 {
		msg += " On " + u.Spec.Version + ": " + strings.Join(upgraded, ", ") + "."
	}
	if len(behind) > 0 {
		msg += " Still on " + fromKubernetes(u) + ": " + strings.Join(behind, ", ") + "."
	}
	msg += " " + upgrades.RestoreHint(snapshotOf(u), fromKubernetes(u))
	k.finish(u, kwerftv1.UpgradeFailed, reason, msg)
	log.FromContext(ctx).Info("kubernetes upgrade failed", "upgrade", u.Name, "node", node, "reason", reason)
	return k.writeLog(ctx, u, nil, "Failed: "+sentence(cause))
}

func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

// writeLog appends lines (time-stamped) to the Upgrade's log ConfigMap,
// key install.log, which the API shows as for a Kwerft upgrade, and sets
// extra keys.
func (k *KubernetesUpgrader) writeLog(ctx context.Context, u *kwerftv1.Upgrade, extra map[string]string, lines ...string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: upgrades.LogConfigMapName(u.Name), Namespace: k.ns()}}
	err := k.Client.Get(ctx, client.ObjectKeyFromObject(cm), cm)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	var b strings.Builder
	b.WriteString(cm.Data[upgrades.LogKey])
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %s\n", k.now().UTC().Format(time.RFC3339), l)
	}
	text := b.String()
	if len(text) > upgrades.LogTailBytes {
		text = text[len(text)-upgrades.LogTailBytes:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	cm.Data[upgrades.LogKey] = text
	for key, v := range extra {
		cm.Data[key] = v
	}
	if create {
		cm.Labels = map[string]string{upgrades.LabelUpgrade: u.Name, LabelManagedBy: ManagedByKwerft}
		cm.OwnerReferences = []metav1.OwnerReference{upgrades.UpgradeOwner(u)}
		return k.Client.Create(ctx, cm)
	}
	return k.Client.Update(ctx, cm)
}

// progressLines are the log lines for nodes whose state changed.
func progressLines(before, after []kwerftv1.UpgradeNode) []string {
	var out []string
	for _, n := range after {
		i := slices.IndexFunc(before, func(o kwerftv1.UpgradeNode) bool { return o.Name == n.Name })
		if i >= 0 && before[i].State == n.State && before[i].Version == n.Version {
			continue
		}
		line := fmt.Sprintf("%s: %s (%s)", n.Name, n.State, n.Version)
		if n.Message != "" {
			line += " " + n.Message
		}
		out = append(out, line)
	}
	return out
}
