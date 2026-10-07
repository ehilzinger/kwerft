// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"net/http"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/store"
)

func projectSpec(t *testing.T, name string) kwerftv1.ProjectSpec {
	t.Helper()
	var p kwerftv1.Project
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return p.Spec
}

func TestProjectAccessAPI(t *testing.T) {
	c := newConsole(t)
	const name = "pa-shop"
	c.project(t, name)
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	sam := c.signIn(t, "Sam@Example.com", store.RoleDeveloper)
	admin := c.signIn(t, "pa-admin@example.com", store.RoleAdmin)
	base := "/api/v1/projects/" + name

	// Team by default; the project list says so, with the user's role.
	var projects []projectJSON
	c.dev.do(t, "GET", "/api/v1/projects", nil, &projects)
	if i := slices.IndexFunc(projects, func(p projectJSON) bool { return p.Name == name }); i < 0 || projects[i].Access != "Team" || projects[i].Role != "developer" {
		t.Fatalf("projects = %+v", projects)
	}

	// Validation, before anything is written.
	for _, tc := range []struct {
		why   string
		body  any
		field string
	}{
		{"unknown account", map[string]any{"access": "Members", "members": []map[string]string{{"user": "ghost@example.com", "role": "developer"}}}, "user"},
		{"owners reach everything", map[string]any{"access": "Members", "members": []map[string]string{{"user": "owner@example.com", "role": "viewer"}}}, "user"},
		{"no such role", map[string]any{"access": "Members", "members": []map[string]string{{"user": "viewer@example.com", "role": "admin"}}}, "role"},
		{"listed twice", map[string]any{"access": "Members", "members": []map[string]string{{"user": "viewer@example.com", "role": "viewer"}, {"user": "VIEWER@example.com", "role": "developer"}}}, "members"},
		{"no such access", map[string]any{"access": "Everyone"}, "access"},
	} {
		var e apiError
		if code := c.owner.do(t, "PUT", base+"/access", tc.body, &e); code != http.StatusUnprocessableEntity || e.Field != tc.field {
			t.Errorf("%s: %d %+v, want 422 on %s", tc.why, code, e, tc.field)
		}
	}
	if code := c.owner.do(t, "POST", base+"/members", map[string]string{"user": "viewer@example.com", "role": "viewer"}, nil); code != http.StatusConflict {
		t.Errorf("add a member to a Team project: %d, want 409", code)
	}

	// Switching to Members with who keeps access; the account's own spelling
	// of the address is stored (Kubernetes compares user names exactly).
	var out projectAccessJSON
	if code := c.owner.do(t, "PUT", base+"/access", map[string]any{"access": "Members", "members": []map[string]string{{"user": "sam@example.com", "role": "viewer"}}}, &out); code != http.StatusOK {
		t.Fatalf("switch to Members: %d %+v", code, out)
	}
	if out.Access != "Members" || len(out.Members) != 1 || out.Members[0].User != "Sam@Example.com" || out.Members[0].Name != "Sam" {
		t.Errorf("access = %+v", out)
	}
	waitForProjectBindings(t, name)
	if code := c.dev.do(t, "GET", base+"/apps/web", nil, nil); code != http.StatusForbidden {
		t.Errorf("developer outside the members reads an app: %d, want 403", code)
	}
	if code := sam.do(t, "POST", base+"/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusForbidden {
		t.Errorf("sam, a viewer here, deploys: %d, want 403", code)
	}

	// Change sam's role, add the viewer, remove sam.
	for _, m := range []map[string]string{{"user": "sam@example.com", "role": "developer"}, {"user": "viewer@example.com", "role": "viewer"}} {
		if code := c.owner.do(t, "POST", base+"/members", m, &out); code != http.StatusOK {
			t.Fatalf("add %v: %d", m, code)
		}
	}
	if got := projectSpec(t, name).Members; len(got) != 2 || got[0].Role != "developer" {
		t.Errorf("members = %+v", got)
	}
	waitForProjectBindings(t, name)
	if code := sam.do(t, "POST", base+"/apps", imageApp("web", "nginx:1.27"), nil); code != http.StatusCreated {
		t.Errorf("sam, now a developer here, deploys: %d", code)
	}
	if code := admin.do(t, "DELETE", base+"/members/viewer@example.com", nil, &out); code != http.StatusOK || len(out.Members) != 1 {
		t.Errorf("admin removes the viewer: %d %+v", code, out)
	}
	if code := c.owner.do(t, "DELETE", base+"/members/nobody@example.com", nil, nil); code != http.StatusNotFound {
		t.Errorf("remove a non-member: %d", code)
	}

	// Removing sam's console account takes them off the project too.
	u := mustUser(t, c.store, "Sam@Example.com")
	if code := c.owner.do(t, "DELETE", "/api/v1/members/"+u.ID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("remove sam's account: %d", code)
	}
	if got := projectSpec(t, name).Members; len(got) != 0 {
		t.Errorf("members after the account is gone = %+v", got)
	}

	// Back to Team clears the list.
	if code := c.owner.do(t, "PUT", base+"/access", map[string]any{"access": "Team"}, &out); code != http.StatusOK || out.Access != "Team" {
		t.Errorf("back to Team: %d %+v", code, out)
	}
	entries, err := c.store.RecentAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action+" "+e.Target)
	}
	for _, want := range []string{"project.access_changed " + name, "project.member_added " + name, "project.member_removed " + name} {
		if !slices.Contains(actions, want) {
			t.Errorf("audit lacks %q", want)
		}
	}
}
