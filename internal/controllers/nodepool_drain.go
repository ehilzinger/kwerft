package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Draining and removing nodes, shared by the node pool reconciler (Cloud
// servers, from the management cluster) and the node removal reconciler
// (any node, in its own cluster).
//
// Drain behaviour (docs/phase5.md › As built (W2)):
//   - Cordon, then evict every pod through the Eviction API, so
//     PodDisruptionBudgets are respected. DaemonSet and static (mirror) pods
//     stay; finished pods are deleted.
//   - Pods whose eviction a PDB refuses are retried on the next pass. After
//     DrainTimeout (default 15 min, like GKE's node pool upgrades but
//     shorter) they are deleted anyway: a pool shrinking or a server being
//     replaced must not hang forever on a PDB that can never be met.
//   - A node holding local volumes (local-path PVs bound to it) is not
//     drained at all unless the removal is forced: the data lives on that
//     server's disk and would be gone with it.
//   - A pod that is terminating counts as gone once its grace period is
//     over (on a dead node nothing ever confirms it).

// Annotations on Kubernetes Nodes.
const (
	// AnnotationNodeRemove asks Kwerft to drain and remove a node: "true",
	// or "force" to go ahead despite local volumes. The console writes it
	// as the signed-in user.
	AnnotationNodeRemove = "kwerft.dev/remove"
	// AnnotationNodeDrain asks Kwerft to cordon and drain a node but keep it.
	AnnotationNodeDrain = "kwerft.dev/drain"
	// AnnotationDrainStarted is when Kwerft began draining (RFC 3339).
	AnnotationDrainStarted = "kwerft.dev/drain-started"
	// AnnotationDrainStatus is Kwerft's last word on a drain.
	AnnotationDrainStatus = "kwerft.dev/drain-status"

	// k3s removes a server from embedded etcd when its Node carries this
	// annotation, and records the member it removed in the other one
	// (k3s pkg/etcd member controller).
	annotationEtcdRemove  = "etcd.k3s.cattle.io/remove"
	annotationEtcdRemoved = "etcd.k3s.cattle.io/removed-node-name"
	// annotationEtcdRemoveStarted is when Kwerft asked k3s for it.
	annotationEtcdRemoveStarted = "kwerft.dev/etcd-remove-started"

	// LabelControlPlane is set by k3s on its servers.
	LabelControlPlane = "node-role.kubernetes.io/control-plane"

	// DefaultDrainTimeout is how long PDBs may hold up a drain.
	DefaultDrainTimeout = 15 * time.Minute
	// DefaultEtcdRemoveTimeout is how long to wait for k3s to remove an etcd
	// member before deleting the node anyway.
	DefaultEtcdRemoveTimeout = 3 * time.Minute
)

// IsControlPlane reports whether a node runs the Kubernetes control plane.
func IsControlPlane(n *corev1.Node) bool {
	return n.Labels[LabelControlPlane] == "true" || n.Labels["node-role.kubernetes.io/master"] == "true"
}

// NodeReady reports the node's Ready condition.
func NodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodeReadySince is when the Ready condition last changed (zero if none).
func nodeReadyChanged(n *corev1.Node) time.Time {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// drainStarted is when the drain began, or zero.
func drainStarted(n *corev1.Node) time.Time {
	t, err := time.Parse(time.RFC3339, n.Annotations[AnnotationDrainStarted])
	if err != nil {
		return time.Time{}
	}
	return t
}

// startDrain cordons the node and records when draining began.
func startDrain(ctx context.Context, c client.Client, n *corev1.Node, now time.Time) error {
	if n.Spec.Unschedulable && !drainStarted(n).IsZero() {
		return nil
	}
	patch := client.MergeFrom(n.DeepCopy())
	n.Spec.Unschedulable = true
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	if drainStarted(n).IsZero() {
		n.Annotations[AnnotationDrainStarted] = now.UTC().Format(time.RFC3339)
	}
	return c.Patch(ctx, n, patch)
}

// setDrainStatus records a short message on the node, when it changed.
func setDrainStatus(ctx context.Context, c client.Client, n *corev1.Node, msg string) error {
	if n.Annotations[AnnotationDrainStatus] == msg {
		return nil
	}
	patch := client.MergeFrom(n.DeepCopy())
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[AnnotationDrainStatus] = msg
	return c.Patch(ctx, n, patch)
}

// localVolumes lists the PersistentVolumes pinned to the node (local-path
// and other local volumes), as "namespace/claim".
func localVolumes(ctx context.Context, c client.Reader, node string) ([]string, error) {
	var pvs corev1.PersistentVolumeList
	if err := c.List(ctx, &pvs); err != nil {
		return nil, err
	}
	var out []string
	for _, pv := range pvs.Items {
		if pv.Spec.ClaimRef == nil || pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
			continue
		}
		for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
			for _, e := range term.MatchExpressions {
				if e.Key == corev1.LabelHostname && e.Operator == corev1.NodeSelectorOpIn && slices.Contains(e.Values, node) {
					out = append(out, pv.Spec.ClaimRef.Namespace+"/"+pv.Spec.ClaimRef.Name)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// drainResult says how a drain pass went.
type drainResult struct {
	Done    bool
	Message string
}

// drain makes one pass over the node's pods: evict what can go, delete
// finished pods, and report whether the node is empty. The node must be
// cordoned (startDrain) first. force skips the local-volume check.
// Lists go through r (uncached: pods by node), changes through c.
func drain(ctx context.Context, r client.Reader, c client.Client, n *corev1.Node, now time.Time, timeout time.Duration, force bool) (drainResult, error) {
	if !force {
		vols, err := localVolumes(ctx, r, n.Name)
		if err != nil {
			return drainResult{}, err
		}
		if len(vols) > 0 {
			return drainResult{Message: fmt.Sprintf("Holds local volumes (%s); their data would be lost. Move them, or remove the node with force.",
				strings.Join(vols, ", "))}, nil
		}
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{"spec.nodeName": n.Name}); err != nil {
		return drainResult{}, err
	}
	overdue := timeout > 0 && !drainStarted(n).IsZero() && now.Sub(drainStarted(n)) > timeout
	var blocked, waiting []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror || ownedByDaemonSet(p) {
			continue
		}
		name := p.Namespace + "/" + p.Name
		if p.DeletionTimestamp != nil {
			if now.Before(p.DeletionTimestamp.Time) {
				waiting = append(waiting, name)
			}
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			if err := c.Delete(ctx, p); client.IgnoreNotFound(err) != nil {
				return drainResult{}, err
			}
			continue
		}
		if overdue {
			// Past the timeout: PDBs no longer hold the node.
			if err := c.Delete(ctx, p); client.IgnoreNotFound(err) != nil {
				return drainResult{}, err
			}
			waiting = append(waiting, name)
			continue
		}
		ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
		err := c.SubResource("eviction").Create(ctx, p, ev)
		switch {
		case err == nil, apierrors.IsNotFound(err):
			waiting = append(waiting, name)
		case apierrors.IsTooManyRequests(err):
			blocked = append(blocked, name)
		default:
			return drainResult{}, fmt.Errorf("evict %s: %w", name, err)
		}
	}
	switch {
	case len(blocked) > 0:
		left := ""
		if timeout > 0 && !drainStarted(n).IsZero() {
			left = fmt.Sprintf("; they are deleted anyway after %s", humanDuration(timeout))
		}
		return drainResult{Message: fmt.Sprintf("Waiting for disruption budgets to allow evicting %s%s.", short(blocked), left)}, nil
	case len(waiting) > 0:
		return drainResult{Message: fmt.Sprintf("Waiting for %s to stop.", short(waiting))}, nil
	}
	return drainResult{Done: true, Message: "Drained."}, nil
}

func ownedByDaemonSet(p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// short names up to three items and counts the rest.
func short(items []string) string {
	slices.Sort(items)
	if len(items) <= 3 {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:3], ", "), len(items)-3)
}

// removeEtcdMember asks k3s to take a control-plane node out of embedded
// etcd and reports whether it is out (or the wait timed out).
func removeEtcdMember(ctx context.Context, c client.Client, n *corev1.Node, now time.Time, timeout time.Duration) (bool, error) {
	if !IsControlPlane(n) {
		return true, nil
	}
	if n.Annotations[annotationEtcdRemoved] != "" {
		return true, nil
	}
	started, err := time.Parse(time.RFC3339, n.Annotations[annotationEtcdRemoveStarted])
	if n.Annotations[annotationEtcdRemove] != "true" || err != nil {
		patch := client.MergeFrom(n.DeepCopy())
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[annotationEtcdRemove] = "true"
		n.Annotations[annotationEtcdRemoveStarted] = now.UTC().Format(time.RFC3339)
		n.Annotations[AnnotationDrainStatus] = "Removing the etcd member."
		return false, c.Patch(ctx, n, patch)
	}
	// k3s answers within seconds; a dead node's member may need the
	// timeout, after which deleting the node lets k3s clean up.
	return now.Sub(started) > timeout, nil
}

// CheckControlPlaneRemoval says whether a control-plane node may be removed: never
// the last one, and only while every other control-plane node is Ready, so
// etcd keeps its quorum.
func CheckControlPlaneRemoval(nodes []corev1.Node, victim string) error {
	var others, ready int
	for i := range nodes {
		n := &nodes[i]
		if n.Name == victim || !IsControlPlane(n) {
			continue
		}
		others++
		if NodeReady(n) {
			ready++
		}
	}
	switch {
	case others == 0:
		return fmt.Errorf("%s is the cluster's last control-plane node", victim)
	case ready < others:
		return fmt.Errorf("another control-plane node is not Ready; removing %s now could cost etcd its quorum", victim)
	}
	return nil
}
