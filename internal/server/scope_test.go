package server

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/store"
)

func TestScopeOf(t *testing.T) {
	project := func(name string, access kwerftv1.ProjectAccess, members ...kwerftv1.ProjectMember) kwerftv1.Project {
		return kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: kwerftv1.ProjectSpec{Access: access, Members: members}}
	}
	projects := []kwerftv1.Project{
		project("legacy", ""), // made before Phase 4: Team
		project("team", kwerftv1.ProjectAccessTeam),
		project("shop", kwerftv1.ProjectAccessMembers, kwerftv1.ProjectMember{User: "Dana@example.com", Role: "viewer"}),
		project("secret", kwerftv1.ProjectAccessMembers),
		project("pending", kwerftv1.ProjectAccessTeam), // namespace not made yet
		project("squat", kwerftv1.ProjectAccessTeam),   // a namespace Kwerft does not manage
	}
	owned := map[string]bool{"legacy": true, "team": true, "shop": true, "secret": true}

	for _, tc := range []struct {
		who                 *store.User
		reaches, namespaces []string
		roles               map[string]string
		platform, all       bool
	}{
		{
			who:     &store.User{Email: "Dana@example.com", Role: store.RoleDeveloper},
			reaches: []string{"legacy", "pending", "shop", "squat", "team"}, namespaces: []string{"legacy", "shop", "team"},
			roles: map[string]string{"shop": "viewer", "team": "developer", "legacy": "developer"},
		},
		{
			// Kubernetes compares user names exactly, and so does the scope.
			who:     &store.User{Email: "dana@example.com", Role: store.RoleDeveloper},
			reaches: []string{"legacy", "pending", "squat", "team"}, namespaces: []string{"legacy", "team"},
		},
		{
			who:     &store.User{Email: "ada@example.com", Role: store.RoleAdmin},
			reaches: []string{"legacy", "pending", "secret", "shop", "squat", "team"}, namespaces: []string{"legacy", "secret", "shop", "team"},
			roles: map[string]string{"secret": "admin"}, platform: true, all: true,
		},
	} {
		s := scopeOf(tc.who, projects, owned)
		var reached []string
		for _, p := range projects {
			if s.reaches(p.Name) {
				reached = append(reached, p.Name)
			}
		}
		slices.Sort(reached)
		if !slices.Equal(reached, tc.reaches) || !slices.Equal(s.namespaces(), tc.namespaces) || s.platform != tc.platform || s.reachesAll() != tc.all {
			t.Errorf("%s: reaches %v, namespaces %v, platform %v, all %v", tc.who.Email, reached, s.namespaces(), s.platform, s.reachesAll())
		}
		for p, want := range tc.roles {
			if got := s.role(p); got != want {
				t.Errorf("%s: role in %s = %q, want %q", tc.who.Email, p, got, want)
			}
		}
		if s.has("kube-system") != tc.platform || s.has("secret") != (tc.platform) {
			t.Errorf("%s: has(kube-system) = %v, has(secret) = %v", tc.who.Email, s.has("kube-system"), s.has("secret"))
		}
	}

	// An API token limited to projects: never more, never the platform.
	s := scopeOf(&store.User{Email: "ada@example.com", Role: store.RoleOwner}, projects, owned)
	s.restrict([]string{"shop", "elsewhere"})
	if s.platform || !slices.Equal(s.namespaces(), []string{"shop"}) || s.has("kube-system") || s.has("team") || s.reachesAll() {
		t.Errorf("restricted owner: platform %v, namespaces %v", s.platform, s.namespaces())
	}
	d := scopeOf(&store.User{Email: "dana@example.com", Role: store.RoleDeveloper}, projects, owned)
	d.restrict([]string{"secret"})
	if len(d.namespaces()) != 0 || d.reaches("secret") {
		t.Errorf("a restriction widened the scope: %v", d.namespaces())
	}
}
