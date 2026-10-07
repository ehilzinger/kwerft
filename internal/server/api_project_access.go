// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/access"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Project access (docs/phase4.md): who besides owners and admins reaches a
// project. Team, the default, is every console member with their console
// role; Members is only the users listed on the Project, each with the role
// given there (developer or viewer).
//
//	GET    /api/v1/projects/{project}/access           → {project, access, members}
//	PUT    /api/v1/projects/{project}/access           {access, members: [{user, role}]}
//	POST   /api/v1/projects/{project}/members          {user, role}  (add, or change the role)
//	DELETE /api/v1/projects/{project}/members/{user}
//
// Owners and admins manage it. The console checks the role first for a
// clear answer; the write itself is an update of the Project as the user
// (impersonated), so Kubernetes decides, and every change is audited. The
// Project reconciler turns the spec into RoleBindings
// (controllers.ProjectBindings). Members are console accounts, named by their
// email address exactly as the account has it, because Kubernetes compares
// the user name kwerft:<email> byte for byte.

func (a *api) registerProjectAccess(mux *http.ServeMux) {
	manage := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireRole(h, access.RolesWith(access.ProjectAccess)...)))
	}
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(manage(h)) }
	mux.HandleFunc("GET /api/v1/projects/{project}/access", manage(a.projectAccessGet))
	mux.HandleFunc("PUT /api/v1/projects/{project}/access", write(a.projectAccessSet))
	mux.HandleFunc("POST /api/v1/projects/{project}/members", write(a.projectMemberAdd))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/members/{user}", write(a.projectMemberRemove))
}

type projectAccessJSON struct {
	Project string              `json:"project"`
	Access  string              `json:"access"`
	Members []projectMemberJSON `json:"members"`
}

type memberInput struct {
	User string `json:"user"`
	Role string `json:"role"`
}

// projectMembers lists a project's members with their names (names: email →
// console name; a member without an account keeps an empty name).
func projectMembers(p *kwerftv1.Project, names map[string]string) []projectMemberJSON {
	out := make([]projectMemberJSON, 0, len(p.Spec.Members))
	for _, m := range p.Spec.Members {
		out = append(out, projectMemberJSON{User: m.User, Name: names[m.User], Role: m.Role})
	}
	slices.SortFunc(out, func(x, y projectMemberJSON) int { return strings.Compare(x.User, y.User) })
	return out
}

func (a *api) accessView(ctx context.Context, p *kwerftv1.Project) (projectAccessJSON, error) {
	users, err := a.store.Members(ctx)
	if err != nil {
		return projectAccessJSON{}, err
	}
	names := map[string]string{}
	for _, u := range users {
		names[u.Email] = u.Name
	}
	return projectAccessJSON{Project: p.Name, Access: projectAccess(p), Members: projectMembers(p, names)}, nil
}

func projectNotFound(name string) string { return fmt.Sprintf("Project %q not found.", name) }

func (a *api) projectAccessGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var proj kwerftv1.Project
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &proj); err != nil {
		a.kubeError(w, r, p, "project.access", name, projectNotFound(name), err)
		return
	}
	out, err := a.accessView(ctx, &proj)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// errMember is a member the console refuses, with the field to show it at.
type errMember struct{ field, msg string }

func (e *errMember) Error() string { return e.msg }

// resolveMember checks one member against the console's accounts and
// returns it with the account's spelling of the address.
func (a *api) resolveMember(ctx context.Context, in memberInput) (kwerftv1.ProjectMember, error) {
	email := strings.TrimSpace(in.User)
	if in.Role != store.RoleDeveloper && in.Role != store.RoleViewer {
		return kwerftv1.ProjectMember{}, &errMember{"role", "Choose developer or viewer."}
	}
	u, err := a.store.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) || email == "" {
		return kwerftv1.ProjectMember{}, &errMember{"user", fmt.Sprintf("No one with the address %q is a member of this console. Invite them first.", email)}
	}
	if err != nil {
		return kwerftv1.ProjectMember{}, err
	}
	if u.Role == store.RoleOwner || u.Role == store.RoleAdmin {
		return kwerftv1.ProjectMember{}, &errMember{"user", fmt.Sprintf("%s is %s and reaches every project already.", u.Email, article(u.Role))}
	}
	return kwerftv1.ProjectMember{User: u.Email, Role: in.Role}, nil
}

func article(role string) string {
	if role == store.RoleOwner || role == store.RoleAdmin {
		return "an " + role
	}
	return "a " + role
}

// memberError answers a refused member, or an internal error.
func (a *api) memberError(w http.ResponseWriter, r *http.Request, err error) {
	var em *errMember
	if errors.As(err, &em) {
		invalid(w, em.field, em.msg)
		return
	}
	a.internalError(w, r, err)
}

// describeMembers is the audit detail for a member list.
func describeMembers(ms []kwerftv1.ProjectMember) string {
	if len(ms) == 0 {
		return "no members"
	}
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.User+" ("+m.Role+")")
	}
	return strings.Join(parts, ", ")
}

// changeProject updates the Project as the user. change sees the current
// object (fresh again after a conflict) and reports false to write nothing;
// a change that leaves the spec as it was writes nothing either.
func (a *api) changeProject(w http.ResponseWriter, r *http.Request, action string, change func(*kwerftv1.Project) bool) (*kwerftv1.Project, bool) {
	name := r.PathValue("project")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return nil, false
	}
	var proj kwerftv1.Project
	for attempt := 0; ; attempt++ {
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &proj); err != nil {
			a.kubeError(w, r, p, action, name, projectNotFound(name), err)
			return nil, false
		}
		before := proj.Spec.DeepCopy()
		if !change(&proj) || equality.Semantic.DeepEqual(*before, proj.Spec) {
			return &proj, true
		}
		err := c.Update(ctx, &proj)
		if err == nil {
			return &proj, true
		}
		if !apierrors.IsConflict(err) || attempt == 4 {
			a.kubeError(w, r, p, action, name, projectNotFound(name), err)
			return nil, false
		}
		proj = kwerftv1.Project{} // decoding into a used object keeps stale fields
	}
}

func (a *api) projectAccessSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Access  string        `json:"access"`
		Members []memberInput `json:"members"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	var members []kwerftv1.ProjectMember
	switch kwerftv1.ProjectAccess(req.Access) {
	case kwerftv1.ProjectAccessTeam:
		// Members only count with access Members; dropping them means a
		// later switch back starts from a fresh choice, never from stale
		// grants.
	case kwerftv1.ProjectAccessMembers:
		if len(req.Members) > 200 {
			invalid(w, "members", "A project has at most 200 members.")
			return
		}
		seen := map[string]bool{}
		for _, in := range req.Members {
			m, err := a.resolveMember(ctx, in)
			if err != nil {
				a.memberError(w, r, err)
				return
			}
			if seen[m.User] {
				invalid(w, "members", m.User+" is listed twice.")
				return
			}
			seen[m.User] = true
			members = append(members, m)
		}
	default:
		invalid(w, "access", "Choose Team or Members.")
		return
	}
	pr := principalOf(r)
	var before string
	proj, ok := a.changeProject(w, r, "project.access", func(p *kwerftv1.Project) bool {
		before = projectAccess(p)
		p.Spec.Access = kwerftv1.ProjectAccess(req.Access)
		p.Spec.Members = members
		return true
	})
	if !ok {
		return
	}
	detail := before + " → " + req.Access
	if req.Access == string(kwerftv1.ProjectAccessMembers) {
		detail += ": " + describeMembers(members)
	}
	a.audit(r, pr.user.Email, "project.access_changed", proj.Name, detail)
	a.writeAccess(w, r, proj)
}

func (a *api) writeAccess(w http.ResponseWriter, r *http.Request, proj *kwerftv1.Project) {
	out, err := a.accessView(r.Context(), proj)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var errTeamProject = errors.New("team project")

func (a *api) projectMemberAdd(w http.ResponseWriter, r *http.Request) {
	var in memberInput
	if !decode(w, r, &in) {
		return
	}
	m, err := a.resolveMember(r.Context(), in)
	if err != nil {
		a.memberError(w, r, err)
		return
	}
	pr := principalOf(r)
	var refused error
	var previous string
	proj, ok := a.changeProject(w, r, "project.member_add", func(p *kwerftv1.Project) bool {
		refused, previous = nil, ""
		if p.Spec.Access != kwerftv1.ProjectAccessMembers {
			refused = errTeamProject
			return false
		}
		i := slices.IndexFunc(p.Spec.Members, func(x kwerftv1.ProjectMember) bool { return x.User == m.User })
		switch {
		case i >= 0:
			previous = p.Spec.Members[i].Role
			p.Spec.Members[i].Role = m.Role
		case len(p.Spec.Members) >= 200:
			refused = &errMember{"user", "A project has at most 200 members."}
			return false
		default:
			p.Spec.Members = append(p.Spec.Members, m)
		}
		return true
	})
	if !ok {
		return
	}
	switch { // when refused, changeProject wrote nothing
	case errors.Is(refused, errTeamProject):
		writeError(w, http.StatusConflict, "Every console member reaches this project (access Team). Limit it to its members first.")
		return
	case refused != nil:
		a.memberError(w, r, refused)
		return
	}
	detail := m.User + " as " + m.Role
	if previous != "" && previous != m.Role {
		detail = m.User + ": " + previous + " → " + m.Role
	}
	if previous != m.Role {
		a.audit(r, pr.user.Email, "project.member_added", proj.Name, detail)
	}
	a.writeAccess(w, r, proj)
}

func (a *api) projectMemberRemove(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	pr := principalOf(r)
	removed := ""
	proj, ok := a.changeProject(w, r, "project.member_remove", func(p *kwerftv1.Project) bool {
		removed = ""
		p.Spec.Members = slices.DeleteFunc(p.Spec.Members, func(m kwerftv1.ProjectMember) bool {
			if strings.EqualFold(m.User, user) {
				removed = m.User + " (" + m.Role + ")"
				return true
			}
			return false
		})
		return removed != ""
	})
	if !ok {
		return
	}
	if removed == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s is not a member of project %q.", user, proj.Name))
		return
	}
	a.audit(r, pr.user.Email, "project.member_removed", proj.Name, removed)
	a.writeAccess(w, r, proj)
}

// dropFromProjects takes a removed console account off every project's
// member list, as the owner or admin who removed it, so that a new account
// with the same address later does not inherit the access. Best effort: a
// failure is logged; the account is gone and cannot sign in either way.
func (a *api) dropFromProjects(r *http.Request, actor *principal, email string) {
	if a.cfg.Kube == nil {
		return
	}
	// Every connected cluster; one that cannot be reached now keeps the
	// address in its member lists (logged), harmless while no account has it.
	for _, st := range a.clusters.states() {
		if st.conn == nil {
			a.cfg.Logger.Error("could not remove a member from the projects of an unreachable cluster", "cluster", st.name, "member", email)
			continue
		}
		a.dropFromClusterProjects(r.WithContext(withCluster(r.Context(), st.conn, true)), actor, email)
	}
}

func (a *api) dropFromClusterProjects(r *http.Request, actor *principal, email string) {
	c, err := a.conn(r.Context()).kube.For(actor.user.Email, actor.user.Role)
	if err != nil {
		a.cfg.Logger.Error("could not remove a member from projects", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	var projects kwerftv1.ProjectList
	if err := a.list(ctx, c, &projects); err != nil {
		a.cfg.Logger.Error("could not remove a member from projects", "err", err)
		return
	}
	for i := range projects.Items {
		p := &projects.Items[i]
		if !slices.ContainsFunc(p.Spec.Members, func(m kwerftv1.ProjectMember) bool { return m.User == email }) {
			continue
		}
		var fresh kwerftv1.Project
		if err := c.Get(ctx, client.ObjectKey{Name: p.Name}, &fresh); err != nil {
			a.cfg.Logger.Error("could not remove a member from a project", "project", p.Name, "err", err)
			continue
		}
		err := updateSpec(ctx, c, &fresh, func(pr *kwerftv1.Project) bool {
			pr.Spec.Members = slices.DeleteFunc(pr.Spec.Members, func(m kwerftv1.ProjectMember) bool { return m.User == email })
			return true
		})
		if err != nil {
			a.cfg.Logger.Error("could not remove a member from a project", "project", p.Name, "err", err)
			continue
		}
		a.audit(r, actor.user.Email, "project.member_removed", p.Name, email+" (account removed)")
	}
}
