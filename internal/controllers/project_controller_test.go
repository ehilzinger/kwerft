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

func TestProjectBindsPodAccessForConsoleRoles(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	p := createProject(t, "pod-access", kwerftv1.ProjectSpec{})

	want := map[string][]string{
		PodsReadRole: {"kwerft:role:owner", "kwerft:role:admin", "kwerft:role:developer", "kwerft:role:viewer"},
		PodsExecRole: {"kwerft:role:owner", "kwerft:role:admin", "kwerft:role:developer"},
	}
	for role, groups := range want {
		var rb rbacv1.RoleBinding
		eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Namespace: "pod-access", Name: role}, &rb) })
		if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != role {
			t.Errorf("%s: roleRef = %+v", role, rb.RoleRef)
		}
		var got []string
		for _, s := range rb.Subjects {
			if s.Kind != rbacv1.GroupKind {
				t.Errorf("%s: subject %+v is not a group", role, s)
			}
			got = append(got, s.Name)
		}
		if fmt.Sprint(got) != fmt.Sprint(groups) {
			t.Errorf("%s: subjects = %v, want %v", role, got, groups)
		}
		if !metav1.IsControlledBy(&rb, p) {
			t.Errorf("%s: not controlled by the Project", role)
		}
	}
}
