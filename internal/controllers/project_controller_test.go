package controllers

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	werftv1 "github.com/ehilzinger/werft/api/v1alpha1"
)

func createProject(t *testing.T, name string, spec werftv1.ProjectSpec) *werftv1.Project {
	t.Helper()
	p := &werftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectCreatesManagedNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	p := createProject(t, "storefront", werftv1.ProjectSpec{PodSecurity: "restricted"})

	var ns corev1.Namespace
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Name: "storefront"}, &ns) })

	want := map[string]string{
		LabelProject:                         "storefront",
		LabelManagedBy:                       ManagedByWerft,
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
	p := createProject(t, "quota-test", werftv1.ProjectSpec{
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
	p := createProject(t, "open", werftv1.ProjectSpec{Isolated: ptr.To(false)})
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
	p := createProject(t, "legacy", werftv1.ProjectSpec{})

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
		t.Error("Werft labelled a namespace it does not own")
	}
}
