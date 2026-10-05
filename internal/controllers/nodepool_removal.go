package controllers

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// NodeRemovalReconciler drains and removes nodes on request, in the
// cluster it runs in (every cluster runs Kwerft's controllers). The console
// asks by annotating the Node as the signed-in user:
//
//   - kwerft.dev/drain=true: cordon and drain, keep the node (the console's
//     "Uncordon" takes both back);
//   - kwerft.dev/remove=true|force: drain, take a control-plane node out of
//     etcd, delete the Node. The machine itself is the owner's to switch off
//     (install.sh --uninstall), or k3s registers it again.
//
// Nodes of a node pool (label kwerft.dev/pool) are removed by the node pool
// reconciler instead, which deletes their Cloud server too.
type NodeRemovalReconciler struct {
	client.Client
	// APIReader lists pods by node and volumes, uncached.
	APIReader client.Reader
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// DrainTimeout and EtcdRemoveTimeout; zero means the defaults.
	DrainTimeout, EtcdRemoveTimeout time.Duration
}

func (r *NodeRemovalReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *NodeRemovalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	asked := predicate.NewPredicateFuncs(func(o client.Object) bool {
		a := o.GetAnnotations()
		return a[AnnotationNodeRemove] != "" || a[AnnotationNodeDrain] != ""
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(asked)).
		Named("noderemoval").
		Complete(r)
}

func (r *NodeRemovalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var n corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &n); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	remove := n.Annotations[AnnotationNodeRemove]
	keep := n.Annotations[AnnotationNodeDrain] != ""
	if remove == "" && !keep {
		return ctrl.Result{}, nil
	}
	if remove != "" && n.Labels[hetzner.LabelPool] != "" {
		return ctrl.Result{}, nil // the node pool reconciler's
	}
	now := r.now()
	if remove != "" && IsControlPlane(&n) {
		var nodes corev1.NodeList
		if err := r.List(ctx, &nodes); err != nil {
			return ctrl.Result{}, err
		}
		if err := CheckControlPlaneRemoval(nodes.Items, n.Name); err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, setDrainStatus(ctx, r.Client, &n, "Waiting: "+err.Error()+".")
		}
	}
	if err := startDrain(ctx, r.Client, &n, now); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	res, err := drain(ctx, r.APIReader, r.Client, &n, now, orDuration(r.DrainTimeout, DefaultDrainTimeout), remove == "force" || !NodeReady(&n))
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := setDrainStatus(ctx, r.Client, &n, res.Message); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !res.Done {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if remove == "" {
		return ctrl.Result{}, nil // drained; the node stays cordoned
	}
	out, err := removeEtcdMember(ctx, r.Client, &n, now, orDuration(r.EtcdRemoveTimeout, DefaultEtcdRemoveTimeout))
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !out {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &n))
}
