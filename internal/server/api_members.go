// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ehilzinger/kwerft/internal/access"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/kube"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Team membership: invites, accepting them, roles and removal, the role
// matrix and the audit log behind the Access page.
//
// Who may do what comes from internal/access. Owners manage everyone; admins
// manage everyone but owners (they can neither invite an owner nor change or
// remove one). The last owner can never be demoted or removed, which the
// store checks inside the same transaction as the change.
//
// A role change needs no propagation step: every request reads the user's
// role from the database with the session, the impersonating Kubernetes
// clients are cached per email *and* role, and long-lived streams (logs,
// shells) end when the role they started with no longer holds.

const (
	inviteTTL = 7 * 24 * time.Hour
	// Invite links carry 256 random bits, so guessing is hopeless; the limit
	// keeps it that way and keeps the audit log readable.
	inviteGuesses = 20
)

// InviteSender delivers invite links, e.g. by email. Kwerft has none yet:
// without one (Config.InviteSender nil) the link is shown once to the person
// who created the invite, to pass on themselves. An SMTP sender plugs in
// here; the store already records delivery (Invite.SentAt).
type InviteSender interface {
	SendInvite(ctx context.Context, msg InviteMessage) error
}

// InviteMessage is what a sender needs to write the email.
type InviteMessage struct {
	To        string
	Role      string
	InvitedBy string // name of the person who invited
	URL       string // the single-use link; never log it
	Expires   time.Time
}

type membersAPI struct {
	*api
	guesses *limiter // invite token lookups per IP
}

var errOwnerOnly = errors.New("only an owner may do this")

func (a *api) registerMembers(mux *http.ServeMux) {
	m := &membersAPI{api: a, guesses: newLimiter(inviteGuesses, 15*time.Minute, a.now)}
	may := func(perm string, h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireRole(h, access.RolesWith(perm)...))
	}
	manage := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(may(access.ManageMembers, h)) }

	mux.HandleFunc("GET /api/v1/roles", a.requireUser(m.roles))
	mux.HandleFunc("GET /api/v1/members", may(access.ManageMembers, m.list))
	mux.HandleFunc("PATCH /api/v1/members/{id}", manage(m.setRole))
	mux.HandleFunc("DELETE /api/v1/members/{id}", manage(m.remove))
	mux.HandleFunc("POST /api/v1/members/{id}/reset-second-factor", manage(m.resetSecondFactor))
	mux.HandleFunc("GET /api/v1/sign-in-policy", a.requireUser(m.policyGet))
	mux.HandleFunc("PUT /api/v1/sign-in-policy", a.sameOrigin(may(access.RequireTwoFactor, m.policySet)))
	mux.HandleFunc("GET /api/v1/invites", may(access.ManageMembers, m.invites))
	mux.HandleFunc("POST /api/v1/invites", manage(m.invite))
	mux.HandleFunc("POST /api/v1/invites/{id}/reissue", manage(m.reissue))
	mux.HandleFunc("DELETE /api/v1/invites/{id}", manage(m.revoke))
	// Public: the invite page. The token travels in the body, never in a URL
	// the server logs.
	mux.HandleFunc("POST /api/v1/invite/lookup", a.sameOrigin(m.lookup))
	mux.HandleFunc("POST /api/v1/invite/accept", a.sameOrigin(m.accept))

	mux.HandleFunc("GET /api/v1/audit", may(access.ReadAudit, m.auditLog))
	mux.HandleFunc("GET /api/v1/audit/facets", may(access.ReadAudit, m.auditFacets))
}

// mayAssign reports whether actor may give or take away role: owners anything,
// admins (partial "members" grant) anything but owner.
func mayAssign(actor *store.User, role string) bool {
	if actor.Role == store.RoleOwner {
		return true
	}
	return access.Allowed(actor.Role, access.ManageMembers) && role != store.RoleOwner
}

func principalOf(r *http.Request) *principal { return r.Context().Value(ctxKey{}).(*principal) }

// ---- roles -------------------------------------------------------------------

func (m *membersAPI) roles(w http.ResponseWriter, _ *http.Request) {
	type roleJSON struct {
		Role        string `json:"role"`
		Group       string `json:"group"`
		ClusterRole string `json:"clusterRole"`
		// ProjectRole is bound in each project namespace the role reaches
		// (controllers.ProjectBindings); owners and admins need none.
		ProjectRole string `json:"projectRole,omitempty"`
	}
	roles := make([]roleJSON, 0, len(access.Roles))
	for _, r := range access.Roles {
		j := roleJSON{Role: r, Group: kube.RoleGroup(r), ClusterRole: "kwerft:" + r}
		if r == access.Developer || r == access.Viewer {
			j.ProjectRole = "kwerft:project-" + r
		}
		roles = append(roles, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": roles, "permissions": access.Matrix})
}

// ---- members -----------------------------------------------------------------

func (m *membersAPI) list(w http.ResponseWriter, r *http.Request) {
	me := principalOf(r).user
	list, err := m.store.Members(r.Context())
	if err != nil {
		m.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, mem := range list {
		factors := []string{}
		if mem.Passkeys > 0 {
			factors = append(factors, methodPasskey)
		}
		if mem.TOTP {
			factors = append(factors, methodTOTP)
		}
		var last any
		if !mem.LastActive.IsZero() {
			last = mem.LastActive.UTC().Format(time.RFC3339)
		}
		out = append(out, map[string]any{
			"id": mem.ID, "name": mem.Name, "email": mem.Email, "role": mem.Role,
			"createdAt": mem.CreatedAt.UTC().Format(time.RFC3339), "lastActive": last,
			"secondFactor": factors, "you": mem.ID == me.ID,
			"manageable": mayAssign(me, mem.Role),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *membersAPI) setRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	me := principalOf(r).user
	if !access.Valid(req.Role) {
		writeFieldError(w, "role", "Choose one of owner, admin, developer or viewer.")
		return
	}
	before, err := m.store.SetRole(r.Context(), r.PathValue("id"), req.Role, func(cur *store.User) error {
		if !mayAssign(me, cur.Role) || !mayAssign(me, req.Role) {
			return errOwnerOnly
		}
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "That member no longer exists. Reload the page.")
		return
	case errors.Is(err, errOwnerOnly):
		writeError(w, http.StatusForbidden, "Only an owner can make someone an owner or change an owner's role.")
		return
	case errors.Is(err, store.ErrLastOwner):
		writeError(w, http.StatusConflict, "This is the only owner. Make someone else an owner first.")
		return
	case err != nil:
		m.internalError(w, r, err)
		return
	}
	if before.Role != req.Role {
		m.audit(r, me.Email, "member.role_changed", before.Email, before.Role+" → "+req.Role)
	}
	after := *before
	after.Role = req.Role
	writeJSON(w, http.StatusOK, userJSON(&after))
}

func (m *membersAPI) remove(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	me := p.user
	gone, sessions, err := m.store.DeleteUser(r.Context(), r.PathValue("id"), func(cur *store.User) error {
		if !mayAssign(me, cur.Role) {
			return errOwnerOnly
		}
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "That member no longer exists. Reload the page.")
		return
	case errors.Is(err, errOwnerOnly):
		writeError(w, http.StatusForbidden, "Only an owner can remove an owner.")
		return
	case errors.Is(err, store.ErrLastOwner):
		writeError(w, http.StatusConflict, "This is the only owner. Make someone else an owner before removing this account.")
		return
	case err != nil:
		m.internalError(w, r, err)
		return
	}
	m.mfa.dropPendingFor(gone.ID) // a half-finished sign-in must not complete
	m.audit(r, me.Email, "member.removed", gone.Email, fmt.Sprintf("was %s; %d sessions signed out", gone.Role, sessions))
	m.dropFromProjects(r, p, gone.Email)
	if gone.ID == me.ID {
		m.clearCookie(w, m.cookies.session)
	}
	w.WriteHeader(http.StatusNoContent)
}

// resetSecondFactor removes a member's authenticator app, passkeys and
// recovery codes, e.g. after a lost phone, and signs them out everywhere.
// They sign in with their password next and set up a factor again (at once,
// when the console requires one). Owners and admins may, admins not for
// owners; nobody for themselves (the Account page manages one's own).
func (m *membersAPI) resetSecondFactor(w http.ResponseWriter, r *http.Request) {
	me := principalOf(r).user
	if r.PathValue("id") == me.ID {
		writeError(w, http.StatusConflict, "Manage your own second factors on your Account page.")
		return
	}
	u, had, sessions, err := m.store.ResetSecondFactors(r.Context(), r.PathValue("id"), func(cur *store.User) error {
		if !mayAssign(me, cur.Role) {
			return errOwnerOnly
		}
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "That member no longer exists. Reload the page.")
		return
	case errors.Is(err, errOwnerOnly):
		writeError(w, http.StatusForbidden, "Only an owner can reset an owner's second factors.")
		return
	case err != nil:
		m.internalError(w, r, err)
		return
	}
	m.mfa.dropPendingFor(u.ID) // a sign-in waiting for the old factor must not complete
	var gone []string
	if had.TOTP {
		gone = append(gone, "authenticator app")
	}
	if had.Passkeys > 0 {
		gone = append(gone, fmt.Sprintf("%d passkeys", had.Passkeys))
	}
	if had.RecoveryCodes > 0 {
		gone = append(gone, fmt.Sprintf("%d recovery codes", had.RecoveryCodes))
	}
	if len(gone) == 0 {
		gone = append(gone, "none set up")
	}
	m.audit(r, me.Email, "member.second_factor_reset", u.Email, fmt.Sprintf("%s; %d sessions signed out", strings.Join(gone, ", "), sessions))
	writeJSON(w, http.StatusOK, map[string]any{"signedOut": sessions})
}

// ---- sign-in policy -------------------------------------------------------------

// policyGet tells everyone whether a second factor is required (the Account
// page explains it); policySet is for owners. Turning the requirement on
// needs a second factor of one's own, so the owner who flips it is never
// the first one sent to enrol; nobody is locked out either way, because a
// member without a factor still signs in with their password and is then
// sent to set one up (requireUser).
func (m *membersAPI) policyGet(w http.ResponseWriter, r *http.Request) {
	on, err := m.store.RequireTwoFactor(r.Context())
	if err != nil {
		m.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"requireTwoFactor": on})
}

func (m *membersAPI) policySet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequireTwoFactor *bool `json:"requireTwoFactor"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.RequireTwoFactor == nil {
		writeFieldError(w, "requireTwoFactor", "Say whether a second factor is required (true or false).")
		return
	}
	ctx, me := r.Context(), principalOf(r).user
	on := *req.RequireTwoFactor
	if on {
		f, err := m.store.Factors(ctx, me.ID)
		if err != nil {
			m.internalError(w, r, err)
			return
		}
		if !f.Any() {
			writeError(w, http.StatusConflict, "Set up a passkey or an authenticator app for your own account first.")
			return
		}
	}
	was, err := m.store.RequireTwoFactor(ctx)
	if err != nil {
		m.internalError(w, r, err)
		return
	}
	if err := m.store.SetRequireTwoFactor(ctx, on); err != nil {
		m.internalError(w, r, err)
		return
	}
	if was != on {
		m.audit(r, me.Email, "settings.require_two_factor", "sign-in", map[bool]string{true: "on", false: "off"}[on])
	}
	writeJSON(w, http.StatusOK, map[string]bool{"requireTwoFactor": on})
}

// ---- invites -----------------------------------------------------------------

func inviteJSON(inv *store.Invite, now time.Time) map[string]any {
	return map[string]any{
		"id": inv.ID, "email": inv.Email, "role": inv.Role,
		"invitedBy": inv.InvitedByName, "invitedByEmail": inv.InvitedByEmail,
		"createdAt": inv.CreatedAt.UTC().Format(time.RFC3339), "expiresAt": inv.ExpiresAt.UTC().Format(time.RFC3339),
		"state": inv.State(now), "sent": !inv.SentAt.IsZero(),
	}
}

func (m *membersAPI) invites(w http.ResponseWriter, r *http.Request) {
	list, err := m.store.OpenInvites(r.Context())
	if err != nil {
		m.internalError(w, r, err)
		return
	}
	now := m.now()
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		j := inviteJSON(&list[i], now)
		j["manageable"] = mayAssign(principalOf(r).user, list[i].Role)
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// inviteURL is the link for a token. With a console domain it is always on
// that domain, so a forged Host header cannot end up in an email.
func (m *membersAPI) inviteURL(r *http.Request, token string) string {
	base := ""
	if d := m.cfg.ConsoleDomain; d != "" && d != "localhost" {
		base = "https://" + d
	} else {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/invite/" + token
}

// deliver hands the link to the invite sender, if there is one. The link is
// returned to the inviter either way, so a failed email is never a dead end.
func (m *membersAPI) deliver(r *http.Request, inv *store.Invite, url string) bool {
	if m.cfg.InviteSender == nil {
		return false
	}
	err := m.cfg.InviteSender.SendInvite(r.Context(), InviteMessage{
		To: inv.Email, Role: inv.Role, InvitedBy: inv.InvitedByName, URL: url, Expires: inv.ExpiresAt,
	})
	if err != nil {
		m.cfg.Logger.Error("invite email failed", "to", inv.Email, "err", err)
		return false
	}
	if err := m.store.MarkInviteSent(r.Context(), inv.ID, m.now()); err != nil {
		m.cfg.Logger.Error("could not record the invite email", "err", err)
	}
	inv.SentAt = m.now()
	return true
}

func (m *membersAPI) invite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	me := principalOf(r).user
	req.Email = strings.TrimSpace(req.Email)
	if !validEmail(req.Email) {
		writeFieldError(w, "email", "Enter a valid email address, like sam@example.com.")
		return
	}
	if !access.Valid(req.Role) {
		writeFieldError(w, "role", "Choose one of owner, admin, developer or viewer.")
		return
	}
	if !mayAssign(me, req.Role) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Only an owner can invite another owner.", "field": "role"})
		return
	}
	token, now := auth.NewToken(), m.now()
	inv := &store.Invite{
		Email: req.Email, Role: req.Role, TokenHash: auth.HashToken(token),
		InvitedByEmail: me.Email, InvitedByName: me.Name, CreatedAt: now, ExpiresAt: now.Add(inviteTTL),
	}
	switch err := m.store.CreateInvite(r.Context(), inv); {
	case errors.Is(err, store.ErrEmailTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": req.Email + " already has an account.", "field": "email"})
		return
	case errors.Is(err, store.ErrInvitePending):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": req.Email + " already has an open invite. Re-issue it from the list to get a new link.", "field": "email",
		})
		return
	case err != nil:
		m.internalError(w, r, err)
		return
	}
	url := m.inviteURL(r, token)
	m.deliver(r, inv, url)
	m.audit(r, me.Email, "member.invite", inv.Email, "as "+inv.Role)
	writeJSON(w, http.StatusCreated, map[string]any{"invite": inviteJSON(inv, now), "url": url})
}

func (m *membersAPI) inviteChangeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "That invite was already accepted or revoked. Reload the page.")
	case errors.Is(err, errOwnerOnly):
		writeError(w, http.StatusForbidden, "Only an owner can change an invite for an owner.")
	default:
		m.internalError(w, r, err)
	}
}

func (m *membersAPI) reissue(w http.ResponseWriter, r *http.Request) {
	me := principalOf(r).user
	token, now := auth.NewToken(), m.now()
	inv, err := m.store.ReissueInvite(r.Context(), r.PathValue("id"), auth.HashToken(token), now, now.Add(inviteTTL), func(inv *store.Invite) error {
		if !mayAssign(me, inv.Role) {
			return errOwnerOnly
		}
		return nil
	})
	if err != nil {
		m.inviteChangeError(w, r, err)
		return
	}
	inv.CreatedAt, inv.ExpiresAt, inv.SentAt = now, now.Add(inviteTTL), time.Time{}
	url := m.inviteURL(r, token)
	m.deliver(r, inv, url)
	m.audit(r, me.Email, "member.invite_reissued", inv.Email, "as "+inv.Role)
	writeJSON(w, http.StatusOK, map[string]any{"invite": inviteJSON(inv, now), "url": url})
}

func (m *membersAPI) revoke(w http.ResponseWriter, r *http.Request) {
	me := principalOf(r).user
	inv, err := m.store.RevokeInvite(r.Context(), r.PathValue("id"), m.now(), func(inv *store.Invite) error {
		if !mayAssign(me, inv.Role) {
			return errOwnerOnly
		}
		return nil
	})
	if err != nil {
		m.inviteChangeError(w, r, err)
		return
	}
	m.audit(r, me.Email, "member.invite_revoked", inv.Email, "as "+inv.Role)
	w.WriteHeader(http.StatusNoContent)
}

// ---- accepting an invite (public) ----------------------------------------------

// openInvite resolves a presented token to an invite that can still be
// accepted, answering the request itself (and returning nil) otherwise.
func (m *membersAPI) openInvite(w http.ResponseWriter, r *http.Request, token string) *store.Invite {
	if !m.guesses.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "Too many attempts. Wait 15 minutes and try again.")
		return nil
	}
	token = strings.TrimSpace(token)
	inv, err := m.store.InviteByTokenHash(r.Context(), auth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) || token == "" {
		m.audit(r, "anonymous", "member.invite_rejected", "invite", "unknown token")
		writeError(w, http.StatusNotFound, "This invite link is not valid. Check that you copied all of it, or ask for a new one.")
		return nil
	}
	if err != nil {
		m.internalError(w, r, err)
		return nil
	}
	switch inv.Err(m.now()) {
	case store.ErrInviteUsed:
		writeError(w, http.StatusGone, "This invite was already used. Sign in instead.")
		return nil
	case store.ErrInviteRevoked:
		writeError(w, http.StatusGone, "This invite was withdrawn. Ask "+inv.InvitedByName+" for a new one.")
		return nil
	case store.ErrInviteExpired:
		writeError(w, http.StatusGone, "This invite expired on "+inv.ExpiresAt.UTC().Format("2 Jan 2006")+". Ask "+inv.InvitedByName+" to re-issue it.")
		return nil
	}
	if _, err := m.store.UserByEmail(r.Context(), inv.Email); err == nil {
		writeError(w, http.StatusConflict, inv.Email+" already has an account. Sign in instead.")
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		m.internalError(w, r, err)
		return nil
	}
	return inv
}

func (m *membersAPI) lookup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	inv := m.openInvite(w, r, req.Token)
	if inv == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"email": inv.Email, "role": inv.Role, "invitedBy": inv.InvitedByName,
		"expiresAt": inv.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (m *membersAPI) accept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	inv := m.openInvite(w, r, req.Token)
	if inv == nil {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeFieldError(w, "name", "Enter your name.")
		return
	}
	if utf8.RuneCountInString(name) > 100 {
		writeFieldError(w, "name", "Use at most 100 characters for your name.")
		return
	}
	if err := auth.CheckPassword(req.Password); err != nil {
		writeFieldError(w, "password", "Password too short: "+err.Error()+".")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		m.internalError(w, r, err)
		return
	}
	u := &store.User{Name: name, PasswordHash: hash}
	// The store checks the invite again in the transaction that creates the
	// account: of two racing accepts, one wins.
	inv, err = m.store.AcceptInvite(r.Context(), auth.HashToken(req.Token), u, m.now())
	switch {
	case errors.Is(err, store.ErrEmailTaken):
		writeError(w, http.StatusConflict, inv.Email+" already has an account. Sign in instead.")
		return
	case errors.Is(err, store.ErrInviteUsed), errors.Is(err, store.ErrInviteRevoked), errors.Is(err, store.ErrInviteExpired), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusGone, "This invite can no longer be used. Ask for a new one.")
		return
	case err != nil:
		m.internalError(w, r, err)
		return
	}
	m.audit(r, u.Email, "member.invite_accepted", u.Email, "as "+u.Role+", invited by "+inv.InvitedByEmail)
	if err := m.startSession(w, r, u); err != nil {
		m.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, userJSON(u))
}

// ---- audit log ---------------------------------------------------------------

const maxAuditPage = 200

func (m *membersAPI) auditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := store.AuditQuery{Actor: q.Get("actor"), Action: q.Get("action"), Limit: 50}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number.")
			return
		}
		query.Limit = min(n, maxAuditPage)
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "before must be an entry ID from a previous page.")
			return
		}
		query.Before = n
	}
	page, err := m.store.QueryAudit(r.Context(), query)
	if err != nil {
		m.internalError(w, r, err)
		return
	}
	entries := make([]map[string]any, 0, len(page.Entries))
	for _, e := range page.Entries {
		entries = append(entries, map[string]any{
			"id": e.ID, "at": e.At.UTC().Format(time.RFC3339), "actor": e.Actor, "action": e.Action,
			"target": e.Target, "ip": e.IP, "detail": e.Detail,
		})
	}
	var next any
	if page.Next > 0 {
		next = page.Next
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": next})
}

func (m *membersAPI) auditFacets(w http.ResponseWriter, r *http.Request) {
	actors, actions, err := m.store.AuditFacets(r.Context())
	if err != nil {
		m.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"actors": actors, "actions": actions})
}
