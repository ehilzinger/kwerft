package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	werftv1 "github.com/ehilzinger/werft/api/v1alpha1"
)

const (
	quotaName       = "werft-quota"
	defaultDenyName = "werft-default-deny"
)

// ProjectReconciler turns a Project into a namespace with quotas, a Pod
// Security level and, when isolated, a default-deny ingress policy that each
// App then opens selectively.
//
// TODO(phase-4): RoleBindings for project members.
type ProjectReconciler struct {
	client.Client
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p werftv1.Project
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // the namespace is garbage-collected through its owner reference
	}
	orig := p.DeepCopy()

	err := r.reconcile(ctx, &p)
	if err != nil {
		setReady(&p.Status.Conditions, p.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
	} else {
		setReady(&p.Status.Conditions, p.Generation, metav1.ConditionTrue, "Reconciled", "Namespace "+p.Name+" is ready")
	}
	p.Status.ObservedGeneration = p.Generation
	if !equality.Semantic.DeepEqual(orig.Status, p.Status) {
		if perr := r.Status().Patch(ctx, &p, client.MergeFrom(orig)); perr != nil {
			return ctrl.Result{}, perr
		}
	}
	if isTerminal(err) {
		return ctrl.Result{}, nil // retrying will not help; the user has to act
	}
	return ctrl.Result{}, err
}

func (r *ProjectReconciler) reconcile(ctx context.Context, p *werftv1.Project) error {
	// Never adopt a namespace someone else created.
	var existing corev1.Namespace
	switch err := r.Get(ctx, client.ObjectKey{Name: p.Name}, &existing); {
	case err == nil:
		if existing.Labels[LabelProject] != p.Name {
			return terminalf("NamespaceConflict", "namespace %q already exists and is not managed by Werft", p.Name)
		}
	case !apierrors.IsNotFound(err):
		return err
	}

	owner := controllerRef(p, werftv1.GroupVersion.WithKind("Project"))
	level := p.Spec.PodSecurity
	if level == "" {
		level = "baseline"
	}

	ns := corev1ac.Namespace(p.Name).
		WithLabels(map[string]string{
			LabelManagedBy:                       ManagedByWerft,
			LabelProject:                         p.Name,
			"pod-security.kubernetes.io/enforce": level,
			"pod-security.kubernetes.io/audit":   "restricted",
			"pod-security.kubernetes.io/warn":    "restricted",
		}).
		WithOwnerReferences(owner)
	if err := apply(ctx, r.Client, ns); err != nil {
		return fmt.Errorf("apply namespace: %w", err)
	}

	if len(p.Spec.Quota) > 0 {
		quota := corev1ac.ResourceQuota(quotaName, p.Name).
			WithLabels(map[string]string{LabelManagedBy: ManagedByWerft}).
			WithOwnerReferences(owner).
			WithSpec(corev1ac.ResourceQuotaSpec().WithHard(p.Spec.Quota))
		if err := apply(ctx, r.Client, quota); err != nil {
			return fmt.Errorf("apply quota: %w", err)
		}
	} else if err := deleteIfControlledBy(ctx, r.Client, &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: quotaName, Namespace: p.Name}}, p); err != nil {
		return err
	}

	if p.Spec.Isolated == nil || *p.Spec.Isolated {
		deny := networkingv1ac.NetworkPolicy(defaultDenyName, p.Name).
			WithLabels(map[string]string{LabelManagedBy: ManagedByWerft}).
			WithOwnerReferences(owner).
			WithSpec(networkingv1ac.NetworkPolicySpec().
				WithPodSelector(metav1ac.LabelSelector()).
				WithPolicyTypes(networkingv1.PolicyTypeIngress))
		if err := apply(ctx, r.Client, deny); err != nil {
			return fmt.Errorf("apply default-deny policy: %w", err)
		}
	} else if err := deleteIfControlledBy(ctx, r.Client, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: defaultDenyName, Namespace: p.Name}}, p); err != nil {
		return err
	}
	return nil
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&werftv1.Project{}).
		Owns(&corev1.Namespace{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("project").
		Complete(r)
}
