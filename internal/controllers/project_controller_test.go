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

	// Default deny: every pod, one empty ingress rule (allows nothing).
	assertSpec(t, ciliumSpec(t, "storefront", ProjectPolicyName), `
endpointSelector: {}
ingress: [{}]
`)

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
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "open", Name: ProjectPolicyName}, newCilium()); !apierrors.IsNotFound(err) {
		t.Errorf("expected no default-deny policy, got err=%v", err)
	}

	// Turning isolation on adds it; off again removes it.
	setIsolated(t, "open", true)
	ciliumSpec(t, "open", ProjectPolicyName)
	setIsolated(t, "open", false)
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "open", Name: ProjectPolicyName}, newCilium()); !apierrors.IsNotFound(err) {
			return fmt.Errorf("default-deny still there (err=%v)", err)
		}
		return nil
	})
}

// An upgrade from before Phase 4: the Kubernetes NetworkPolicies Kwerft
// wrote give way to CiliumNetworkPolicies; anyone else's stay.
func TestProjectAndAppMigrateToCiliumPolicies(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	p := createProject(t, "legacy-np", kwerftv1.ProjectSpec{})
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			return err
		}
		return k8s.Get(ctx, client.ObjectKey{Name: "legacy-np"}, &corev1.Namespace{})
	})
	app := createApp(t, "legacy-np", "web", kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.29"}},
		Ports:  []kwerftv1.AppPort{{Container: 80}},
	})
	waitForApp(t, app, "Progressing")

	old := func(name string, owner metav1.Object, kind string) *networkingv1.NetworkPolicy {
		np := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "legacy-np"},
			Spec:       networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}},
		}
		if owner != nil {
			np.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, kwerftv1.GroupVersion.WithKind(kind))}
		}
		if err := k8s.Create(ctx, np); err != nil {
			t.Fatal(err)
		}
		return np
	}
	old(defaultDenyName, p, "Project")
	old("web", app, "App")
	old("hand-made", nil, "")

	// Any change makes both reconcile (an upgrade restarts them all).
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.DisplayName = "Legacy"
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	app.Spec.Replicas = ptr.To[int32](2)
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{defaultDenyName, "web"} {
		eventually(t, func() error {
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: "legacy-np", Name: name}, &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
				return fmt.Errorf("NetworkPolicy %s still there (err=%v)", name, err)
			}
			return nil
		})
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "legacy-np", Name: "hand-made"}, &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("a NetworkPolicy Kwerft does not own must stay: %v", err)
	}
	ciliumSpec(t, "legacy-np", ProjectPolicyName)
	ciliumSpec(t, "legacy-np", "web")
}

func setIsolated(t *testing.T, project string, isolated bool) {
	t.Helper()
	ctx := context.Background()
	var p kwerftv1.Project
	if err := k8s.Get(ctx, client.ObjectKey{Name: project}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Isolated = ptr.To(isolated)
	if err := k8s.Update(ctx, &p); err != nil {
		t.Fatal(err)
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
