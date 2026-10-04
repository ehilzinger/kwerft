package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/kube"
)

const (
	quotaName       = "kwerft-quota"
	defaultDenyName = "kwerft-default-deny"

	// ClusterRoles from the chart (roles.yaml) that each project binds:
	// reading pods, logs and pod metrics, and opening a shell. Binding them
	// per namespace keeps every console role out of the platform's own pods.
	PodsReadRole = "kwerft:pods-read"
	PodsExecRole = "kwerft:pods-exec"
	// What developers and viewers may do with the project's kwerft.dev
	// objects (apps, builds, tasks, ...). Owners and admins hold that
	// cluster-wide already.
	ProjectDeveloperRole = "kwerft:project-developer"
	ProjectViewerRole    = "kwerft:project-viewer"
)

// ProjectBinding is one RoleBinding the reconciler keeps in a project
// namespace: the ClusterRole it binds (also the binding's name) and to whom.
type ProjectBinding struct {
	ClusterRole string
	Subjects    []rbacv1.Subject
}

func groupSubject(role string) rbacv1.Subject {
	return rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: kube.RoleGroup(role)}
}

func userSubject(email string) rbacv1.Subject {
	return rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: kube.UserName(email)}
}

// ProjectBindings says who reaches a project, as RoleBindings in its
// namespace (docs/phase4.md, "Project access"). Owners and admins always do,
// through their groups: their cluster-wide role covers the kwerft.dev
// objects, these bindings add pods, logs and shells. With access Team (the
// default) the developer and viewer groups are bound too, as before Phase 4;
// with access Members only the listed users are, each with the role given
// there, whatever their console role. The access matrix tests
// (internal/access) and the isolation suite (internal/server) hold this
// against the chart.
func ProjectBindings(p *kwerftv1.Project) []ProjectBinding {
	var dev, view, read, exec []rbacv1.Subject
	for _, role := range []string{"owner", "admin"} {
		read = append(read, groupSubject(role))
		exec = append(exec, groupSubject(role))
	}
	if p.Spec.Access == kwerftv1.ProjectAccessMembers {
		// Sorted, so a reordered list does not rewrite the bindings.
		members := slices.Clone(p.Spec.Members)
		slices.SortFunc(members, func(a, b kwerftv1.ProjectMember) int { return strings.Compare(a.User, b.User) })
		for _, m := range members {
			s := userSubject(m.User)
			switch m.Role {
			case "developer":
				dev = append(dev, s)
				exec = append(exec, s)
			case "viewer":
				view = append(view, s)
			default:
				continue // the CRD allows nothing else
			}
			read = append(read, s)
		}
	} else {
		dev = append(dev, groupSubject("developer"))
		view = append(view, groupSubject("viewer"))
		read = append(read, groupSubject("developer"), groupSubject("viewer"))
		exec = append(exec, groupSubject("developer"))
	}
	return []ProjectBinding{
		{ProjectDeveloperRole, dev},
		{ProjectViewerRole, view},
		{PodsReadRole, read},
		{PodsExecRole, exec},
	}
}

// ProjectReconciler turns a Project into a namespace with quotas, a Pod
// Security level, RoleBindings that give the console's users access to the
// project (ProjectBindings), and, when isolated, a default-deny ingress
// policy that each App then opens selectively.
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

	// Subjects is an atomic list, so applying it replaces it whole: a member
	// removed from the Project, or a switch from Team to Members, takes the
	// old subjects away.
	for _, b := range ProjectBindings(p) {
		rb := rbacv1ac.RoleBinding(b.ClusterRole, p.Name).
			WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
			WithOwnerReferences(owner).
			WithRoleRef(rbacv1ac.RoleRef().WithAPIGroup(rbacv1.GroupName).WithKind("ClusterRole").WithName(b.ClusterRole))
		for _, s := range b.Subjects {
			rb.WithSubjects(rbacv1ac.Subject().WithAPIGroup(s.APIGroup).WithKind(s.Kind).WithName(s.Name))
		}
		if err := apply(ctx, r.Client, rb); err != nil {
			return fmt.Errorf("apply role binding %s: %w", b.ClusterRole, err)
		}
	}

	if p.Spec.Isolated == nil || *p.Spec.Isolated {
		deny := networkingv1ac.NetworkPolicy(defaultDenyName, p.Name).
			WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
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
		For(&kwerftv1.Project{}).
		Owns(&corev1.Namespace{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&rbacv1.RoleBinding{}).
		Named("project").
		Complete(r)
}
