package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/kube"
)

const (
	quotaName       = "kwerft-quota"
	defaultDenyName = "kwerft-default-deny"

	// ClusterRoles from the chart (roles.yaml) that each project binds for
	// the console's role groups: reading pods, logs and pod metrics, and
	// opening a shell. Binding them per namespace keeps every console role
	// out of the platform's own pods.
	PodsReadRole = "kwerft:pods-read"
	PodsExecRole = "kwerft:pods-exec"
)

// podAccess lists which console roles get each pod ClusterRole in a project.
var podAccess = []struct {
	role  string
	roles []string
}{
	{PodsReadRole, []string{"owner", "admin", "developer", "viewer"}},
	{PodsExecRole, []string{"owner", "admin", "developer"}},
}

// ProjectReconciler turns a Project into a namespace with quotas, a Pod
// Security level, RoleBindings that give the console's roles access to the
// project's pods (logs, shell), and, when isolated, a default-deny ingress
// policy that each App then opens selectively.
//
// TODO(phase-4): RoleBindings for project members.
type ProjectReconciler struct {
	client.Client
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p kwerftv1.Project
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
		if perr := patchStatus(ctx, r.Client, &p, orig); perr != nil {
			return ctrl.Result{}, perr
		}
	}
	if isTerminal(err) {
		return ctrl.Result{}, nil // retrying will not help; the user has to act
	}
	return ctrl.Result{}, err
}

func (r *ProjectReconciler) reconcile(ctx context.Context, p *kwerftv1.Project) error {
	// Never adopt a namespace someone else created.
	var existing corev1.Namespace
	switch err := r.Get(ctx, client.ObjectKey{Name: p.Name}, &existing); {
	case err == nil:
		if existing.Labels[LabelProject] != p.Name {
			return terminalf("NamespaceConflict", "namespace %q already exists and is not managed by Kwerft", p.Name)
		}
	case !apierrors.IsNotFound(err):
		return err
	}

	owner := controllerRef(p, kwerftv1.GroupVersion.WithKind("Project"))
	level := p.Spec.PodSecurity
	if level == "" {
		level = "baseline"
	}

	ns := corev1ac.Namespace(p.Name).
		WithLabels(map[string]string{
			LabelManagedBy:                       ManagedByKwerft,
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
			WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
			WithOwnerReferences(owner).
			WithSpec(corev1ac.ResourceQuotaSpec().WithHard(p.Spec.Quota))
		if err := apply(ctx, r.Client, quota); err != nil {
			return fmt.Errorf("apply quota: %w", err)
		}
	} else if err := deleteIfControlledBy(ctx, r.Client, &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: quotaName, Namespace: p.Name}}, p); err != nil {
		return err
	}

	for _, b := range podAccess {
		rb := rbacv1ac.RoleBinding(b.role, p.Name).
			WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
			WithOwnerReferences(owner).
			WithRoleRef(rbacv1ac.RoleRef().WithAPIGroup(rbacv1.GroupName).WithKind("ClusterRole").WithName(b.role))
		for _, role := range b.roles {
			rb.WithSubjects(rbacv1ac.Subject().WithAPIGroup(rbacv1.GroupName).WithKind(rbacv1.GroupKind).WithName(kube.RoleGroup(role)))
		}
		if err := apply(ctx, r.Client, rb); err != nil {
			return fmt.Errorf("apply role binding %s: %w", b.role, err)
		}
	}

	return r.reconcilePolicy(ctx, p)
}

// reconcilePolicy keeps an isolated project's default-deny
// CiliumNetworkPolicy (traffic_render.go) and removes the Kubernetes
// NetworkPolicy Kwerft wrote before Phase 4, after the new one is in place.
func (r *ProjectReconciler) reconcilePolicy(ctx context.Context, p *kwerftv1.Project) error {
	deny := &unstructured.Unstructured{}
	deny.SetGroupVersionKind(CiliumNetworkPolicyGVK)
	deny.SetName(ProjectPolicyName)
	deny.SetNamespace(p.Name)
	if projectIsolated(p) {
		err := apply(ctx, r.Client, client.ApplyConfigurationFromUnstructured(projectPolicy(p)))
		if meta.IsNoMatchError(err) {
			return terminalf("CiliumMissing", "Cilium's CiliumNetworkPolicy is not installed in this cluster; re-run the installer")
		}
		if err != nil {
			return fmt.Errorf("apply default-deny policy: %w", err)
		}
	} else if err := deleteIfControlledBy(ctx, r.Client, deny, p); err != nil {
		return err
	}
	return deleteIfControlledBy(ctx, r.Client, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: defaultDenyName, Namespace: p.Name}}, p)
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	policy := &metav1.PartialObjectMetadata{}
	policy.SetGroupVersionKind(CiliumNetworkPolicyGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&kwerftv1.Project{}).
		Owns(&corev1.Namespace{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(policy, builder.OnlyMetadata).
		Owns(&rbacv1.RoleBinding{}).
		Named("project").
		Complete(r)
}
