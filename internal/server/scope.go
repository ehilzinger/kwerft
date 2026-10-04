package server

import (
	"context"
	"errors"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Project scope: which projects a console user reaches (docs/phase4.md,
// "Project access"). Kubernetes enforces it through the Project reconciler's
// RoleBindings (controllers.ProjectBindings); everything the console reads
// with more than the user's own rights — the informer cache, VictoriaMetrics,
// VictoriaLogs, Alertmanager, shell recordings — confines itself with this:
//
//   - Owners and admins reach every project and the platform's namespaces.
//   - Everyone else reaches the projects with access Team, with their console
//     role, and the projects with access Members that list them, with the
//     role given there.
//
// Single-object reads and every write go through impersonation instead, so
// Kubernetes decides those; this scope only ever narrows what a list,
// search or stream returns.

// projectScope is what one user reaches.
type projectScope struct {
	// platform: owners and admins, unconfined.
	platform bool
	// roles holds the projects the user reaches, with their role in each.
	roles map[string]string
	// owned are the project namespaces that really are the project's own
	// (labelled for it by the reconciler). Data is read only from those: a
	// Project whose name collides with a namespace Kwerft does not manage
	// must not open that namespace's logs or metrics.
	owned map[string]bool
	// partial: some project exists that the user does not reach.
	partial bool
}

// scopeOf works out a user's scope from the Projects and the namespaces
// labelled as project namespaces.
func scopeOf(u *store.User, projects []kwerftv1.Project, owned map[string]bool) *projectScope {
	s := &projectScope{
		platform: u.Role == store.RoleOwner || u.Role == store.RoleAdmin,
		roles:    map[string]string{},
		owned:    map[string]bool{},
	}
	for i := range projects {
		p := &projects[i]
		role := projectRole(u, p)
		if role == "" {
			s.partial = true
			continue
		}
		s.roles[p.Name] = role
		if owned[p.Name] && !platformNamespace(p.Name) {
			s.owned[p.Name] = true
		}
	}
	return s
}

// projectRole is the user's role in a project, "" when they do not reach it.
func projectRole(u *store.User, p *kwerftv1.Project) string {
	switch {
	case u.Role == store.RoleOwner || u.Role == store.RoleAdmin:
		return u.Role
	case p.Spec.Access == kwerftv1.ProjectAccessMembers:
		// Exactly as the RoleBinding names the user (kube.UserName): the
		// console writes the account's own spelling of the address.
		for _, m := range p.Spec.Members {
			if m.User == u.Email {
				return m.Role
			}
		}
		return ""
	default: // Team, also when unset (Projects made before Phase 4)
		return u.Role
	}
}

// restrict narrows the scope to some projects, e.g. those an API token is
// limited to: never more than the user reaches, and never the platform.
// Empty means no restriction.
func (s *projectScope) restrict(projects []string) {
	if len(projects) == 0 {
		return
	}
	s.platform = false // owners and admins: their projects stay, the platform goes
	for p := range s.roles {
		if !slices.Contains(projects, p) {
			delete(s.roles, p)
			delete(s.owned, p)
			s.partial = true
		}
	}
}

// reaches: the user sees this project in lists.
func (s *projectScope) reaches(project string) bool {
	return s.platform || s.roles[project] != ""
}

// role is the user's role in the project, "" when they do not reach it.
func (s *projectScope) role(project string) string { return s.roles[project] }

// has: the user may see data of this namespace. Owners and admins see every
// namespace, the platform's included.
func (s *projectScope) has(namespace string) bool {
	return s.platform || s.owned[namespace]
}

// namespaces are the project namespaces the user may see data of, sorted.
// For owners and admins these are all project namespaces; callers that also
// show the platform check platform.
func (s *projectScope) namespaces() []string {
	out := make([]string, 0, len(s.owned))
	for ns := range s.owned {
		out = append(out, ns)
	}
	slices.Sort(out)
	return out
}

// reachesAll: there is no project the user does not reach.
func (s *projectScope) reachesAll() bool { return s.platform || !s.partial }

// projectScope reads the user's scope. Projects come from the informer cache
// (every role may list them, see roles.yaml); the namespace labels with the
// console's own identity, since users cannot read namespaces.
func (a *api) projectScope(ctx context.Context, pr *principal) (*projectScope, error) {
	if a.cfg.Kube == nil {
		return nil, errors.New("no Kubernetes cluster")
	}
	c, err := a.cfg.Kube.For(pr.user.Email, pr.user.Role)
	if err != nil {
		return nil, err
	}
	var projects kwerftv1.ProjectList
	if err := a.list(ctx, c, &projects); err != nil {
		return nil, err
	}
	owned, err := a.projectNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	return principalScope(pr, projects.Items, owned), nil
}

// principalScope is scopeOf for a request: a project-restricted API token
// reaches only its projects, never the platform, whatever its user reaches.
// Every scope a handler uses must come from here (or projectScope).
func principalScope(pr *principal, projects []kwerftv1.Project, owned map[string]bool) *projectScope {
	s := scopeOf(pr.user, projects, owned)
	if pr.token != nil && pr.token.Projects != nil {
		s.restrict(pr.token.Projects)
	}
	return s
}

// projectNamespaces are the namespaces the Project reconciler made: labelled
// kwerft.dev/project=<their own name> and not being deleted.
func (a *api) projectNamespaces(ctx context.Context) (map[string]bool, error) {
	var items []corev1.Namespace
	listed := false
	if a.cfg.KubeCache != nil {
		var list corev1.NamespaceList
		err := a.cfg.KubeCache.List(ctx, &list, client.HasLabels{controllers.LabelProject})
		var notStarted *cache.ErrCacheNotStarted
		switch {
		case err == nil:
			items, listed = list.Items, true
		case !errors.As(err, &notStarted):
			return nil, err
		}
	}
	if !listed {
		cs, err := a.cfg.Kube.Self()
		if err != nil {
			return nil, err
		}
		list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: controllers.LabelProject})
		if err != nil {
			return nil, err
		}
		items = list.Items
	}
	out := map[string]bool{}
	for _, ns := range items {
		if ns.Labels[controllers.LabelProject] == ns.Name && ns.DeletionTimestamp == nil {
			out[ns.Name] = true
		}
	}
	return out, nil
}

// requestScope is projectScope for a handler; on false it has answered.
func (a *api) requestScope(w http.ResponseWriter, r *http.Request, ctx context.Context, pr *principal, action string) (*projectScope, bool) {
	s, err := a.projectScope(ctx, pr)
	if err != nil {
		a.kubeError(w, r, pr, action, "", "No projects found.", err)
		return nil, false
	}
	return s, true
}

// scopedList lists a namespaced kind across the user's projects: from the
// informer cache, keeping only what the scope has, or, before the cache has
// started (or without one), as the user, project by project. A namespace
// option outside the scope yields an empty list, as if it had no objects.
func (a *api) scopedList(ctx context.Context, c client.Client, s *projectScope, list client.ObjectList, opts ...client.ListOption) error {
	var lo client.ListOptions
	lo.ApplyOptions(opts)
	if lo.Namespace != "" && !s.has(lo.Namespace) {
		return meta.SetList(list, nil)
	}
	if a.cfg.KubeCache != nil {
		err := a.cfg.KubeCache.List(ctx, list, opts...)
		var notStarted *cache.ErrCacheNotStarted
		if err == nil {
			return keepInScope(list, s)
		}
		if !errors.As(err, &notStarted) {
			return err
		}
	}
	if s.platform || lo.Namespace != "" {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		return keepInScope(list, s)
	}
	var all []runtime.Object
	for _, ns := range s.namespaces() {
		part := list.DeepCopyObject().(client.ObjectList)
		if err := c.List(ctx, part, append(slices.Clone(opts), client.InNamespace(ns))...); err != nil {
			return err
		}
		items, err := meta.ExtractList(part)
		if err != nil {
			return err
		}
		all = append(all, items...)
	}
	return meta.SetList(list, all)
}

// keepInScope drops the items of namespaces outside the scope.
func keepInScope(list client.ObjectList, s *projectScope) error {
	if s.platform {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	kept := items[:0]
	for _, o := range items {
		if obj, ok := o.(client.Object); ok && s.has(obj.GetNamespace()) {
			kept = append(kept, o)
		}
	}
	return meta.SetList(list, kept)
}
