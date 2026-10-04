package access

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/ehilzinger/kwerft/internal/kube"
	"github.com/ehilzinger/kwerft/internal/store"
)

// chartRBAC reads the ClusterRoles and ClusterRoleBindings of the chart's
// roles.yaml, dropping template lines as the API tests do.
func chartRBAC(t *testing.T) (map[string]rbacv1.ClusterRole, []rbacv1.ClusterRoleBinding) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "kwerft", "templates", "roles.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "{{") {
			kept = append(kept, line)
		}
	}
	roles := map[string]rbacv1.ClusterRole{}
	var bindings []rbacv1.ClusterRoleBinding
	for _, doc := range strings.Split(strings.Join(kept, "\n"), "\n---") {
		var head metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatal(err)
		}
		switch head.Kind {
		case "ClusterRole":
			var r rbacv1.ClusterRole
			if err := yaml.UnmarshalStrict([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			roles[r.Name] = r
		case "ClusterRoleBinding":
			var b rbacv1.ClusterRoleBinding
			if err := yaml.UnmarshalStrict([]byte(doc), &b); err != nil {
				t.Fatal(err)
			}
			bindings = append(bindings, b)
		}
	}
	return roles, bindings
}

func matches(list []string, v string) bool {
	return slices.Contains(list, "*") || slices.Contains(list, v)
}

func allows(rules []rbacv1.PolicyRule, c Check) bool {
	res := c.Resource
	if c.Subresource != "" {
		res += "/" + c.Subresource
	}
	for _, r := range rules {
		if matches(r.APIGroups, c.Group) && matches(r.Resources, res) && matches(r.Verbs, c.Verb) {
			return true
		}
	}
	return false
}

// TestMatrixAgreesWithChartRoles holds every Kubernetes-enforced row of the
// matrix against the chart's cluster-wide roles. Pod rows (logs, exec) are
// bound per project namespace by the Project reconciler; the envtest suite in
// internal/server checks those against a real API server
// (TestRoleMatrixMatchesKubernetes).
func TestMatrixAgreesWithChartRoles(t *testing.T) {
	roles, bindings := chartRBAC(t)
	rulesFor := func(role string) []rbacv1.PolicyRule {
		var out []rbacv1.PolicyRule
		for _, b := range bindings {
			for _, s := range b.Subjects {
				if s.Kind == rbacv1.GroupKind && s.Name == kube.RoleGroup(role) {
					cr, ok := roles[b.RoleRef.Name]
					if !ok {
						t.Fatalf("binding %s names missing ClusterRole %s", b.Name, b.RoleRef.Name)
					}
					out = append(out, cr.Rules...)
				}
			}
		}
		return out
	}
	checked := 0
	for _, role := range Roles {
		rules := rulesFor(role)
		if len(rules) == 0 {
			t.Errorf("the chart binds nothing to group %s", kube.RoleGroup(role))
		}
		for _, p := range Matrix {
			for _, c := range p.Kube {
				if c.Group == "" && c.Resource == "pods" {
					continue // per project namespace, see above
				}
				want := p.Grants[role].Level != No
				if got := allows(rules, c); got != want {
					t.Errorf("%s / %q: chart allows %s %s.%s/%s = %v, matrix says %v",
						role, p.Label, c.Verb, c.Resource, c.Group, c.Subresource, got, want)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Kubernetes checks were evaluated")
	}
}

func TestMatrixIsComplete(t *testing.T) {
	if !slices.Equal(Roles, kube.Roles) {
		t.Errorf("access.Roles %v != kube.Roles %v", Roles, kube.Roles)
	}
	for _, r := range []string{store.RoleOwner, store.RoleAdmin, store.RoleDeveloper, store.RoleViewer} {
		if !Valid(r) {
			t.Errorf("store role %s unknown to access", r)
		}
	}
	ids := map[string]bool{}
	for _, p := range Matrix {
		if ids[p.ID] {
			t.Errorf("duplicate permission %s", p.ID)
		}
		ids[p.ID] = true
		for _, r := range Roles {
			if _, ok := p.Grants[r]; !ok {
				t.Errorf("%s: no grant for %s", p.ID, r)
			}
		}
		if (p.Enforced == ByKubernetes) != (len(p.Kube) > 0) {
			t.Errorf("%s: enforced by %s but has %d Kubernetes checks", p.ID, p.Enforced, len(p.Kube))
		}
		if len(RolesWith(p.ID)) > 0 && p.Grants[Owner].Level != Yes {
			t.Errorf("%s: the owner must hold every permission the console grants anyone", p.ID)
		}
	}
	if got := RolesWith(ManageMembers); !slices.Equal(got, []string{Owner, Admin}) {
		t.Errorf("RolesWith(members) = %v", got)
	}
}
