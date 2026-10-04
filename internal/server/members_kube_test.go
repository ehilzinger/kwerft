package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/access"
	"github.com/ehilzinger/kwerft/internal/store"
)

// TestRoleChangeAppliesToKubernetesAtOnce: a role change reaches the
// impersonated identity on the member's very next request, without signing
// in again, in both directions.
func TestRoleChangeAppliesToKubernetesAtOnce(t *testing.T) {
	c := newConsole(t)
	c.project(t, "promote")
	ctx := context.Background()
	viewer, err := c.store.UserByEmail(ctx, "viewer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	dev, err := c.store.UserByEmail(ctx, "developer@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if code := c.viewer.do(t, "POST", "/api/v1/projects/promote/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusForbidden {
		t.Fatalf("viewer creates an app: %d, want 403", code)
	}
	if code := c.owner.do(t, "PATCH", "/api/v1/members/"+viewer.ID, map[string]string{"role": store.RoleDeveloper}, nil); code != http.StatusOK {
		t.Fatalf("promote viewer: %d", code)
	}
	if code := c.viewer.do(t, "POST", "/api/v1/projects/promote/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
		t.Errorf("promoted viewer creates an app: %d, want 201", code)
	}

	if code := c.dev.do(t, "POST", "/api/v1/projects/promote/apps/web/restart", nil, nil); code/100 != 2 {
		t.Fatalf("developer restarts: %d", code)
	}
	if code := c.owner.do(t, "PATCH", "/api/v1/members/"+dev.ID, map[string]string{"role": store.RoleViewer}, nil); code != http.StatusOK {
		t.Fatalf("demote developer: %d", code)
	}
	var e apiError
	if code := c.dev.do(t, "POST", "/api/v1/projects/promote/apps/web/restart", nil, &e); code != http.StatusForbidden {
		t.Errorf("demoted developer restarts: %d %+v, want 403", code, e)
	}
	if code := c.dev.do(t, "DELETE", "/api/v1/projects/promote/apps/web", nil, nil); code != http.StatusForbidden {
		t.Errorf("demoted developer deletes: %d, want 403", code)
	}
	// Still signed in, still reading.
	if code := c.dev.do(t, "GET", "/api/v1/projects/promote/apps/web", nil, nil); code != http.StatusOK {
		t.Errorf("demoted developer reads: %d, want 200", code)
	}
}

// TestRoleMatrixMatchesKubernetes asks the API server, as each role, every
// Kubernetes request the role matrix stands for, in a real project namespace
// (so the Project reconciler's pod bindings count too). The Roles tab cannot
// promise what RBAC does not do, or hide what it allows.
func TestRoleMatrixMatchesKubernetes(t *testing.T) {
	c := newConsole(t)
	c.project(t, "matrix")
	ctx := context.Background()
	// The pod RoleBindings may land just after the namespace: wait for both,
	// so that a "denied" below cannot pass merely because they are missing.
	eventually(t, func() error {
		var rbs rbacv1.RoleBindingList
		if err := cluster.admin.List(ctx, &rbs, client.InNamespace("matrix")); err != nil {
			return err
		}
		if len(rbs.Items) < 2 {
			return fmt.Errorf("%d role bindings in the project namespace", len(rbs.Items))
		}
		return nil
	})
	for _, role := range access.Roles {
		kc, err := cluster.imp.For("matrix-"+role+"@example.com", role)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range access.Matrix {
			for _, check := range p.Kube {
				want := p.Grants[role].Level != access.No
				attrs := &authzv1.ResourceAttributes{Group: check.Group, Resource: check.Resource, Subresource: check.Subresource, Verb: check.Verb}
				if check.Namespaced {
					attrs.Namespace = "matrix"
				}
				review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attrs}}
				if err := kc.Create(ctx, review); err != nil {
					t.Fatal(err)
				}
				if review.Status.Allowed != want {
					t.Errorf("%s: %s %s.%s/%s in %q: Kubernetes allows %v, the matrix says %v (%q)",
						role, check.Verb, check.Resource, check.Group, check.Subresource, attrs.Namespace, review.Status.Allowed, want, p.Label)
				}
			}
		}
	}
}
