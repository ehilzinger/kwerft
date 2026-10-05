package access

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
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

// identity is how a console user reaches Kubernetes.
type identity struct {
	user  string
	group string
}

func consoleUser(email, role string) identity {
	return identity{user: kube.UserName(email), group: kube.RoleGroup(role)}
}

func (id identity) is(s rbacv1.Subject) bool {
	return (s.Kind == rbacv1.UserKind && s.Name == id.user) || (s.Kind == rbacv1.GroupKind && s.Name == id.group)
}

// rbac evaluates the chart's roles plus one project's RoleBindings, as the
// API server would for an identity: cluster-wide rules apply everywhere, the
// project's only in its namespace.
type rbac struct {
	t        *testing.T
	roles    map[string]rbacv1.ClusterRole
	bindings []rbacv1.ClusterRoleBinding
	project  []controllers.ProjectBinding
}

func newRBAC(t *testing.T, p *kwerftv1.Project) *rbac {
	roles, bindings := chartRBAC(t)
	return &rbac{t: t, roles: roles, bindings: bindings, project: controllers.ProjectBindings(p)}
}

func (r *rbac) role(name string) []rbacv1.PolicyRule {
	cr, ok := r.roles[name]
	if !ok {
		r.t.Fatalf("binding names missing ClusterRole %s", name)
	}
	return cr.Rules
}

func (r *rbac) clusterRules(id identity) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, b := range r.bindings {
		if slices.ContainsFunc(b.Subjects, id.is) {
			out = append(out, r.role(b.RoleRef.Name)...)
		}
	}
	return out
}

func (r *rbac) projectRules(id identity) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, b := range r.project {
		if slices.ContainsFunc(b.Subjects, id.is) {
			out = append(out, r.role(b.ClusterRole)...)
		}
	}
	return out
}

func (r *rbac) allowed(id identity, c Check) bool {
	if allows(r.clusterRules(id), c) {
		return true
	}
	return c.Namespaced && allows(r.projectRules(id), c)
}

// TestMatrixAgreesWithChartRoles holds every Kubernetes-enforced row of the
// matrix against the chart's roles and the RoleBindings the Project
// reconciler keeps in a Team project. The envtest suite in internal/server
// asks a real API server the same (TestRoleMatrixMatchesKubernetes).
func TestMatrixAgreesWithChartRoles(t *testing.T) {
	r := newRBAC(t, &kwerftv1.Project{})
	checked := 0
	for _, role := range Roles {
		id := consoleUser(role+"@example.com", role)
		if len(r.clusterRules(id)) == 0 {
			t.Errorf("the chart binds nothing to group %s", kube.RoleGroup(role))
		}
		for _, p := range Matrix {
			for _, c := range p.Kube {
				want := p.Grants[role].Level != No
				if got := r.allowed(id, c); got != want {
					t.Errorf("%s / %q: RBAC allows %s %s.%s/%s = %v, matrix says %v",
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

// TestMembersProjectsFollowTheProjectRole: in a project with access Members,
// a member's project role replaces their console role for everything in the
// namespace, developers and viewers who are not listed reach nothing there,
// and owners and admins are unaffected. Cluster-scoped rows keep following
// the console role.
func TestMembersProjectsFollowTheProjectRole(t *testing.T) {
	r := newRBAC(t, &kwerftv1.Project{Spec: kwerftv1.ProjectSpec{
		Access: kwerftv1.ProjectAccessMembers,
		Members: []kwerftv1.ProjectMember{
			{User: "promoted@example.com", Role: Developer}, // a console viewer
			{User: "reader@example.com", Role: Viewer},      // a console developer
		},
	}})
	for _, tc := range []struct {
		who                    string
		id                     identity
		consoleRole, inProject string // inProject "" = reaches nothing namespaced
	}{
		{"owner", consoleUser("o@example.com", Owner), Owner, Owner},
		{"admin", consoleUser("a@example.com", Admin), Admin, Admin},
		{"viewer listed as developer", consoleUser("promoted@example.com", Viewer), Viewer, Developer},
		{"developer listed as viewer", consoleUser("reader@example.com", Developer), Developer, Viewer},
		{"developer not listed", consoleUser("outsider@example.com", Developer), Developer, ""},
		{"viewer not listed", consoleUser("bystander@example.com", Viewer), Viewer, ""},
	} {
		for _, p := range Matrix {
			for _, c := range p.Kube {
				role := tc.consoleRole
				if c.Namespaced {
					role = tc.inProject
				}
				want := role != "" && p.Grants[role].Level != No
				if got := r.allowed(tc.id, c); got != want {
					t.Errorf("%s / %q: RBAC allows %s %s.%s/%s = %v, want %v",
						tc.who, p.Label, c.Verb, c.Resource, c.Group, c.Subresource, got, want)
				}
			}
		}
	}
}

// chartCRDs are the chart's kwerft.dev CRDs.
func chartCRDs(t *testing.T) []apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "charts", "kwerft", "crds", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs: %v", err)
	}
	var out []apiextensionsv1.CustomResourceDefinition
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatal(err)
		}
		out = append(out, crd)
	}
	return out
}

// namespacedKinds are the plurals of every namespaced kwerft.dev CRD.
func namespacedKinds(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, crd := range chartCRDs(t) {
		if crd.Spec.Scope == apiextensionsv1.NamespaceScoped {
			out = append(out, crd.Spec.Names.Plural)
		}
	}
	return out
}

// TestAdminsReachEveryKindButUpgrades: kwerft:admin lists the kwerft.dev
// kinds instead of "*" so that Upgrades stay owners' to start and cancel.
// Every other kind, and its status and scale, must stay in that list,
// including CRDs added later.
func TestAdminsReachEveryKindButUpgrades(t *testing.T) {
	r := newRBAC(t, &kwerftv1.Project{})
	admin := r.clusterRules(consoleUser("admin@example.com", Admin))
	owner := r.clusterRules(consoleUser("owner@example.com", Owner))
	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	for _, crd := range chartCRDs(t) {
		kind := crd.Spec.Names.Plural
		var subs []string
		for _, v := range crd.Spec.Versions {
			if v.Subresources == nil {
				continue
			}
			if v.Subresources.Status != nil && !slices.Contains(subs, "status") {
				subs = append(subs, "status")
			}
			if v.Subresources.Scale != nil && !slices.Contains(subs, "scale") {
				subs = append(subs, "scale")
			}
		}
		for _, verb := range verbs {
			c := Check{Group: "kwerft.dev", Resource: kind, Verb: verb}
			if !allows(owner, c) {
				t.Errorf("owners cannot %s %s", verb, kind)
			}
			read := verb == "get" || verb == "list" || verb == "watch"
			if want := kind != "upgrades" || read; allows(admin, c) != want {
				t.Errorf("admins %s %s: %v, want %v", verb, kind, !want, want)
			}
			for _, sub := range subs {
				c.Subresource = sub
				if want := kind != "upgrades"; allows(admin, c) != want {
					t.Errorf("admins %s %s/%s: %v, want %v", verb, kind, sub, !want, want)
				}
			}
		}
	}
}

// TestNothingNamespacedIsClusterWideForDevelopersAndViewers: the bindings
// that apply in every namespace never reach a namespaced kwerft.dev kind,
// pods, logs or Secrets for developers and viewers — including CRDs added
// later. Only the Project reconciler's per-namespace bindings do.
func TestNothingNamespacedIsClusterWideForDevelopersAndViewers(t *testing.T) {
	r := newRBAC(t, &kwerftv1.Project{})
	kinds := namespacedKinds(t)
	if !slices.Contains(kinds, "apps") || !slices.Contains(kinds, "builds") {
		t.Fatalf("namespaced kinds %v lack apps or builds", kinds)
	}
	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	for _, role := range []string{Developer, Viewer} {
		rules := r.clusterRules(consoleUser(role+"@example.com", role))
		var checks []Check
		for _, k := range kinds {
			for _, v := range verbs {
				checks = append(checks, Check{Group: "kwerft.dev", Resource: k, Verb: v})
			}
		}
		for _, v := range verbs {
			checks = append(checks,
				Check{Resource: "pods", Verb: v}, Check{Resource: "secrets", Verb: v},
				Check{Resource: "pods", Subresource: "log", Verb: v}, Check{Resource: "pods", Subresource: "exec", Verb: v})
		}
		for _, c := range checks {
			if allows(rules, c) {
				t.Errorf("%s: a cluster-wide binding allows %s %s.%s/%s", role, c.Verb, c.Resource, c.Group, c.Subresource)
			}
		}
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
	if got := RolesWith(ProjectAccess); !slices.Equal(got, []string{Owner, Admin}) {
		t.Errorf("RolesWith(project-access) = %v", got)
	}
	if got := RolesWith(RequireTwoFactor); !slices.Equal(got, []string{Owner}) {
		t.Errorf("RolesWith(require-2fa) = %v", got)
	}
}

// TestDevelopersCannotRewriteBuilds: developers start and cancel builds, but
// a Build is history (and a revision's provenance): no update or delete.
func TestDevelopersCannotRewriteBuilds(t *testing.T) {
	roles, _ := chartRBAC(t)
	dev := roles[controllers.ProjectDeveloperRole].Rules
	for _, verb := range []string{"create", "patch"} {
		if !allows(dev, Check{Group: "kwerft.dev", Resource: "builds", Verb: verb}) {
			t.Errorf("developers cannot %s builds", verb)
		}
	}
	for _, verb := range []string{"update", "delete", "deletecollection"} {
		if allows(dev, Check{Group: "kwerft.dev", Resource: "builds", Verb: verb}) {
			t.Errorf("developers may %s builds", verb)
		}
	}
	if allows(dev, Check{Group: "kwerft.dev", Resource: "builds", Subresource: "status", Verb: "patch"}) {
		t.Error("developers may write build status")
	}
}
