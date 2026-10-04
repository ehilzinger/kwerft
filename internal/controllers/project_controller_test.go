package controllers

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func createProject(t *testing.T, name string, spec kwerftv1.ProjectSpec) *kwerftv1.Project {
	t.Helper()
	p := &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectCreatesManagedNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	p := createProject(t, "storefront", kwerftv1.ProjectSpec{PodSecurity: "restricted"})

	var ns corev1.Namespace
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Name: "storefront"}, &ns) })

	want := map[string]string{
		LabelProject:                         "storefront",
		LabelManagedBy:                       ManagedByKwerft,
		"pod-security.kubernetes.io/enforce": "restricted",
	}
	for k, v := range want {
		if ns.Labels[k] != v {
			t.Errorf("namespace label %s = %q, want %q", k, ns.Labels[k], v)
		}
	}
	if !metav1.IsControlledBy(&ns, p) {
		t.Error("namespace is not controlled by the Project")
	}

	var deny networkingv1.NetworkPolicy
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: "storefront", Name: defaultDenyName}, &deny)
	})
	if len(deny.Spec.Ingress) != 0 || len(deny.Spec.PodSelector.MatchLabels) != 0 {
		t.Errorf("default-deny should select all pods with no ingress rules, got %+v", deny.Spec)
	}

	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			return err
		}
		reason, err := readyReason(p.Status.Conditions, p.Generation)
		if err != nil {
			return err
		}
		if reason != "Reconciled" {
			return fmt.Errorf("reason %q", reason)
		}
		return nil
	})
}

func TestProjectQuotaFollowsSpec(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	p := createProject(t, "quota-test", kwerftv1.ProjectSpec{
		Quota: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("4")},
	})

	var q corev1.ResourceQuota
	key := client.ObjectKey{Namespace: "quota-test", Name: quotaName}
	eventually(t, func() error { return k8s.Get(ctx, key, &q) })
	if got := q.Spec.Hard[corev1.ResourceRequestsCPU]; got.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("quota requests.cpu = %s, want 4", got.String())
	}

	// Removing the quota from the spec removes the ResourceQuota.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Quota = nil
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		err := k8s.Get(ctx, key, &q)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("quota still present (err=%v)", err)
	})
}

func TestProjectWithoutIsolationHasNoDefaultDeny(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	p := createProject(t, "open", kwerftv1.ProjectSpec{Isolated: ptr.To(false)})
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			return err
		}
		_, err := readyReason(p.Status.Conditions, p.Generation)
		return err
	})
	err := k8s.Get(ctx, client.ObjectKey{Namespace: "open", Name: defaultDenyName}, &networkingv1.NetworkPolicy{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no default-deny policy, got err=%v", err)
	}
}

func TestProjectRefusesForeignNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}}); err != nil {
		t.Fatal(err)
	}
	p := createProject(t, "legacy", kwerftv1.ProjectSpec{})

	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			return err
		}
		reason, err := readyReason(p.Status.Conditions, p.Generation)
		if err != nil {
			return err
		}
		if reason != "NamespaceConflict" {
			return fmt.Errorf("reason %q, want NamespaceConflict", reason)
		}
		return nil
	})
	var ns corev1.Namespace
	if err := k8s.Get(ctx, client.ObjectKey{Name: "legacy"}, &ns); err != nil {
		t.Fatal(err)
	}
	if _, touched := ns.Labels[LabelProject]; touched {
		t.Error("Kwerft labelled a namespace it does not own")
	}
}

// expectBinding waits until a project RoleBinding's subjects, as
// "Group:name" / "User:name", equal want.
func expectBinding(t *testing.T, p *kwerftv1.Project, role string, want []string) {
	t.Helper()
	ctx := context.Background()
	var rb rbacv1.RoleBinding
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: p.Name, Name: role}, &rb); err != nil {
			return err
		}
		got := []string{}
		for _, s := range rb.Subjects {
			got = append(got, s.Kind+":"+s.Name)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Errorf("%s: subjects = %v, want %v", role, got, want)
		}
		return nil
	})
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != role {
		t.Errorf("%s: roleRef = %+v", role, rb.RoleRef)
	}
	if !metav1.IsControlledBy(&rb, p) {
		t.Errorf("%s: not controlled by the Project", role)
	}
}

func TestProjectBindsTeamRolesByDefault(t *testing.T) {
	requireEnvtest(t)
	p := createProject(t, "pod-access", kwerftv1.ProjectSpec{})
	const owner, admin, dev, viewer = "Group:kwerft:role:owner", "Group:kwerft:role:admin", "Group:kwerft:role:developer", "Group:kwerft:role:viewer"
	expectBinding(t, p, ProjectDeveloperRole, []string{dev})
	expectBinding(t, p, ProjectViewerRole, []string{viewer})
	expectBinding(t, p, PodsReadRole, []string{owner, admin, dev, viewer})
	expectBinding(t, p, PodsExecRole, []string{owner, admin, dev})
}

// TestProjectMembersReplaceTheTeam: access Members binds the listed users
// with their project role and drops the developer and viewer groups; back to
// Team restores them.
func TestProjectMembersReplaceTheTeam(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	p := createProject(t, "members-only", kwerftv1.ProjectSpec{
		Access: kwerftv1.ProjectAccessMembers,
		Members: []kwerftv1.ProjectMember{
			{User: "vic@example.com", Role: "viewer"},
			{User: "dana@example.com", Role: "developer"},
		},
	})
	const owner, admin = "Group:kwerft:role:owner", "Group:kwerft:role:admin"
	const dana, vic = "User:kwerft:dana@example.com", "User:kwerft:vic@example.com"
	expectBinding(t, p, ProjectDeveloperRole, []string{dana})
	expectBinding(t, p, ProjectViewerRole, []string{vic})
	expectBinding(t, p, PodsReadRole, []string{owner, admin, dana, vic})
	expectBinding(t, p, PodsExecRole, []string{owner, admin, dana})

	// Removing the last developer leaves the binding without subjects.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Members = p.Spec.Members[:1]
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	expectBinding(t, p, ProjectDeveloperRole, []string{})
	expectBinding(t, p, PodsExecRole, []string{owner, admin})
	expectBinding(t, p, PodsReadRole, []string{owner, admin, vic})

	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Access = kwerftv1.ProjectAccessTeam
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	expectBinding(t, p, ProjectViewerRole, []string{"Group:kwerft:role:viewer"})
	expectBinding(t, p, PodsReadRole, []string{owner, admin, "Group:kwerft:role:developer", "Group:kwerft:role:viewer"})
}
