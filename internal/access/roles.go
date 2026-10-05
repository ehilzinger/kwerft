// Package access is the one description of what each console role may do.
// The Access page's role matrix is rendered from it, the console API takes
// its own checks (members, audit log) from it, and tests hold it against the
// Kubernetes RBAC in charts/kwerft/templates/roles.yaml and the Project
// reconciler's per-namespace bindings, so the three cannot drift apart.
//
// Mapping: a console user with role R reaches Kubernetes as user
// "kwerft:<email>" in group "kwerft:role:R" (see internal/kube). The chart
// binds that group cluster-wide to the ClusterRole "kwerft:R": everything in
// kwerft.dev for owners and admins, the cluster-scoped kinds only for
// developers and viewers. Each project namespace adds RoleBindings
// (controllers.ProjectBindings): with access Team (the default) the groups
// get "kwerft:project-developer" / "kwerft:project-viewer", "kwerft:pods-read"
// (all roles) and "kwerft:pods-exec" (owner, admin, developer); with access
// Members the listed users get the same by their project role instead, and
// developers and viewers who are not listed get nothing there.
//
// The matrix describes a Team project. In a Members project a member's
// project role (developer or viewer) takes the place of their console role
// for everything namespaced; owners and admins are unaffected.
package access

import "slices"

// Roles in decreasing power. They match store.Role* and kube.Roles.
const (
	Owner     = "owner"
	Admin     = "admin"
	Developer = "developer"
	Viewer    = "viewer"
)

var Roles = []string{Owner, Admin, Developer, Viewer}

// Level is how far a role holds a permission.
type Level string

const (
	Yes     Level = "yes"
	No      Level = "no"
	Partial Level = "partial" // yes, with the limit in Grant.Note
)

// Grant is one cell of the matrix.
type Grant struct {
	Level Level  `json:"level"`
	Note  string `json:"note,omitempty"`
}

// Check is a Kubernetes request a permission stands for. Kubernetes must
// allow it for exactly the roles whose level is not No.
type Check struct {
	Group       string // API group; "" is the core group
	Resource    string
	Subresource string
	Verb        string
	// Namespaced checks are asked in a project namespace (with access
	// Team); the rest at cluster scope.
	Namespaced bool
}

// Enforcement says who stops a role that lacks the permission.
type Enforcement string

const (
	ByKubernetes Enforcement = "kubernetes" // RBAC on the impersonated identity
	ByConsole    Enforcement = "console"    // the console API
)

// Permission is one row of the matrix.
type Permission struct {
	ID       string           `json:"id"`
	Label    string           `json:"label"`
	Enforced Enforcement      `json:"enforcedBy"`
	Grants   map[string]Grant `json:"grants"` // by role
	Kube     []Check          `json:"-"`
}

var (
	yes = Grant{Level: Yes}
	no  = Grant{Level: No}
)

func all(g Grant) map[string]Grant {
	return map[string]Grant{Owner: g, Admin: g, Developer: g, Viewer: g}
}

// Permission IDs the console checks itself.
const (
	ManageMembers = "members"
	// ProjectAccess: Team or Members, and who the members are. Kubernetes
	// decides (update on the Project); the console asks for owner or admin
	// first.
	ProjectAccess = "project-access"
	// RequireTwoFactor: the console setting that sends everyone without a
	// second factor to enrol after the password.
	RequireTwoFactor = "require-2fa"
	ReadAudit        = "audit"
	SilenceAlerts    = "silence"
	// AlertRules is enforced by Kubernetes, but the console also checks it
	// for Custom rules (owners and admins only).
	AlertRules = "alert-rules"
	// RevealSecrets: read back one value of a secret set, after the
	// password, audited. Kubernetes holds it too (the SecretSet
	// reconciler's Role kwerft:secret-sets-read binds owners and admins).
	RevealSecrets = "reveal-secrets"
	// CopySecretSets: copy a set with its values into another project.
	CopySecretSets = "copy-secret-sets"
)

// Matrix is the source of truth, in the order the Access page shows it.
var Matrix = []Permission{
	{
		ID: "view", Label: "View apps, jobs, pods and logs of the projects they reach", Enforced: ByKubernetes,
		Grants: all(yes),
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "projects", Verb: "list"},
			{Group: "kwerft.dev", Resource: "apps", Verb: "get", Namespaced: true},
			{Group: "kwerft.dev", Resource: "tasks", Verb: "list", Namespaced: true},
			{Resource: "pods", Verb: "list", Namespaced: true},
			{Resource: "pods", Subresource: "log", Verb: "get", Namespaced: true},
		},
	},
	{
		ID: "deploy", Label: "Deploy, change, scale, restart and delete apps", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: yes, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "apps", Verb: "create", Namespaced: true},
			{Group: "kwerft.dev", Resource: "apps", Verb: "patch", Namespaced: true},
			{Group: "kwerft.dev", Resource: "apps", Verb: "delete", Namespaced: true},
		},
	},
	{
		ID: "jobs", Label: "Run jobs, edit schedules and volumes", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: yes, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "tasks", Verb: "create", Namespaced: true},
			{Group: "kwerft.dev", Resource: "schedules", Verb: "update", Namespaced: true},
			{Group: "kwerft.dev", Resource: "volumes", Verb: "create", Namespaced: true},
		},
	},
	{
		ID: "builds", Label: "Build apps from Git and cancel builds", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: yes, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "builds", Verb: "create", Namespaced: true},
			{Group: "kwerft.dev", Resource: "builds", Verb: "patch", Namespaced: true},
		},
	},
	{
		ID: "shell", Label: "Open a shell in a container (recorded)", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: yes, Viewer: no},
		Kube: []Check{
			{Resource: "pods", Subresource: "exec", Verb: "create", Namespaced: true},
		},
	},
	{
		// A TrafficRule lives in its project's namespace, so a developer
		// writes the rules of the projects they work in only.
		ID: "traffic", Label: "Edit traffic rules", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: {Level: Partial, Note: "within their projects"}, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "trafficrules", Verb: "create", Namespaced: true},
			{Group: "kwerft.dev", Resource: "trafficrules", Verb: "update", Namespaced: true},
			{Group: "kwerft.dev", Resource: "trafficrules", Verb: "delete", Namespaced: true},
		},
	},
	{
		// Values are write-only: the SecretSet reconciler's Role grants patch
		// on exactly the sets' Secrets, never get. Viewers see key names.
		ID: "secret-sets", Label: "Create secret sets and set, generate or remove their values (write-only)", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: {Level: Partial, Note: "within their projects; values cannot be read back"}, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "secretsets", Verb: "create", Namespaced: true},
			{Group: "kwerft.dev", Resource: "secretsets", Verb: "update", Namespaced: true},
			{Group: "kwerft.dev", Resource: "secretsets", Verb: "delete", Namespaced: true},
		},
	},
	{
		ID: RevealSecrets, Label: "Reveal a secret value (after their password, audited)", Enforced: ByConsole,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: no, Viewer: no},
	},
	{
		ID: CopySecretSets, Label: "Copy a secret set into another project", Enforced: ByConsole,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: no, Viewer: no},
	},
	{
		ID: "projects", Label: "Create and delete projects", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: no, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "projects", Verb: "create"},
			{Group: "kwerft.dev", Resource: "projects", Verb: "delete"},
		},
	},
	{
		// Members of a project are console users; the project role
		// (developer or viewer) replaces their console role there.
		ID: ProjectAccess, Label: "Limit a project to its members and manage them", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: no, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "projects", Verb: "update"},
			{Group: "kwerft.dev", Resource: "projects", Verb: "patch"},
		},
	},
	{
		// Credentials are write-only even for owners and admins: the
		// GitConnection reconciler grants them patch on each connection's
		// Secret, never get (internal/server checks that against Kubernetes).
		ID: "git", Label: "Manage Git connections and their credentials (write-only)", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: no, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "gitconnections", Verb: "create"},
			{Group: "kwerft.dev", Resource: "gitconnections", Verb: "update"},
			{Group: "kwerft.dev", Resource: "gitconnections", Verb: "delete"},
		},
	},
	{
		// RBAC lets developers write AlertRules; it cannot see spec.expr, so
		// the console keeps Custom rules to owners and admins.
		ID: AlertRules, Label: "Create and change alert rules", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: {Level: Partial, Note: "built-in conditions; not Custom expressions"}, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "alertrules", Verb: "create"},
			{Group: "kwerft.dev", Resource: "alertrules", Verb: "update"},
			{Group: "kwerft.dev", Resource: "alertrules", Verb: "delete"},
		},
	},
	{
		// Like Git credentials: patch on each channel's Secret, never get.
		ID: "alert-channels", Label: "Manage notification channels and their credentials (write-only)", Enforced: ByKubernetes,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: no, Viewer: no},
		Kube: []Check{
			{Group: "kwerft.dev", Resource: "notificationchannels", Verb: "create"},
			{Group: "kwerft.dev", Resource: "notificationchannels", Verb: "update"},
			{Group: "kwerft.dev", Resource: "notificationchannels", Verb: "delete"},
		},
	},
	{
		// Alertmanager has no users; the console decides.
		ID: SilenceAlerts, Label: "Silence alerts", Enforced: ByConsole,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: {Level: Partial, Note: "alerts of projects, not platform alerts"}, Viewer: no},
	},
	{
		ID: "secrets", Label: "Read Kubernetes Secrets (apps reference them by name)", Enforced: ByKubernetes,
		Grants: all(no),
		Kube: []Check{
			{Resource: "secrets", Verb: "get", Namespaced: true},
			{Resource: "secrets", Verb: "list", Namespaced: true},
		},
	},
	{
		ID: ManageMembers, Label: "Invite members, change roles, remove members, reset their second factors", Enforced: ByConsole,
		Grants: map[string]Grant{Owner: yes, Admin: {Level: Partial, Note: "not owners"}, Developer: no, Viewer: no},
	},
	{
		ID: RequireTwoFactor, Label: "Require two-factor sign-in for everyone", Enforced: ByConsole,
		Grants: map[string]Grant{Owner: yes, Admin: no, Developer: no, Viewer: no},
	},
	{
		ID: ReadAudit, Label: "Read the audit log and shell recordings", Enforced: ByConsole,
		Grants: map[string]Grant{Owner: yes, Admin: yes, Developer: no, Viewer: no},
	},
}

// Lookup returns the permission with this ID.
func Lookup(id string) (Permission, bool) {
	for _, p := range Matrix {
		if p.ID == id {
			return p, true
		}
	}
	return Permission{}, false
}

// Allowed reports whether role holds the permission at all (Yes or Partial).
func Allowed(role, id string) bool {
	p, ok := Lookup(id)
	if !ok {
		return false
	}
	g, ok := p.Grants[role]
	return ok && g.Level != No
}

// RolesWith lists the roles that hold the permission at all.
func RolesWith(id string) []string {
	var out []string
	for _, r := range Roles {
		if Allowed(r, id) {
			out = append(out, r)
		}
	}
	return out
}

// Valid reports whether role is one the console knows.
func Valid(role string) bool { return slices.Contains(Roles, role) }
