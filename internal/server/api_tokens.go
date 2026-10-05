package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/store"
)

// API tokens: long-lived credentials for scripts, CI and kubectl, sent as
// "Authorization: Bearer kwft_…" to /api and /k8s.
//
//   - Stored as the SHA-256 of the token (256 random bits, so a fast hash is
//     enough); shown once, at creation.
//   - A role cap: requests act with the lower of the cap and the user's role
//     at the time of the request, so a demotion or removal applies at once.
//   - An optional project restriction (developer or viewer caps only): the
//     console allows only routes of those projects (tokenMayUse), and the
//     Kubernetes proxy only their namespaces (kubeproxy.go). Kubernetes RBAC
//     still applies on top, through impersonation, as for the console.
//   - Expiry: 90 days by default, a year at most.
//   - Tokens manage nothing about identity: no account, tokens, members,
//     invites, single sign-on or data-key routes, and no shells (those are
//     recorded and need a browser session).
//   - Bearer requests skip the same-origin check (no cookies are used) but
//     not rate limits: failed tokens count per IP, and each token has a
//     request budget.

const (
	tokenDefaultTTL = 90 * 24 * time.Hour
	tokenMaxDays    = 365
	maxTokenNameLen = 100
	maxTokenScope   = 20 // projects one token may name
	// tokenTouchEvery throttles last-used writes.
	tokenTouchEvery = time.Minute
	// expired tokens stay listed this long, so their owner sees why a script
	// stopped working; then they are deleted (cleanSessions in main).
	TokenExpiredKeep = 30 * 24 * time.Hour
)

// tokenAuth holds the rate limits of bearer authentication.
type tokenAuth struct {
	failures *limiter // unknown, expired or malformed tokens per IP
	requests *limiter // requests per token
}

func newTokenAuth(now func() time.Time) *tokenAuth {
	return &tokenAuth{
		failures: newLimiter(20, 15*time.Minute, now),
		requests: newLimiter(1200, time.Minute, now),
	}
}

// hasAuthorization reports whether the request carries credentials in the
// Authorization header; such a request is never authenticated by a cookie.
func hasAuthorization(r *http.Request) bool { return r.Header.Get("Authorization") != "" }

// bearerToken returns the bearer credential, if the request has one.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

// roleRank orders roles by power: lower is more.
func roleRank(role string) int {
	switch role {
	case store.RoleOwner:
		return 0
	case store.RoleAdmin:
		return 1
	case store.RoleDeveloper:
		return 2
	case store.RoleViewer:
		return 3
	}
	return 99
}

// effectiveRole is the less powerful of a token's cap and the user's role.
func effectiveRole(tokenRole, userRole string) string {
	if roleRank(tokenRole) > roleRank(userRole) {
		return tokenRole
	}
	return userRole
}

// tokenPrincipal authenticates the request's bearer token. It answers the
// request itself (401, 429) when that fails.
func (a *api) tokenPrincipal(w http.ResponseWriter, r *http.Request) (*principal, bool) {
	ip := clientIP(r)
	if a.tok.failures.exceeded(ip) {
		writeError(w, http.StatusTooManyRequests, "Too many requests with invalid tokens. Wait 15 minutes and try again.")
		return nil, false
	}
	raw, ok := bearerToken(r)
	if !ok || !auth.LooksLikeAPIToken(raw) {
		a.tok.failures.allow(ip)
		writeError(w, http.StatusUnauthorized, "Send an API token as \"Authorization: Bearer kwft_…\". Create one under Account › API tokens.")
		return nil, false
	}
	now := a.now()
	t, u, err := a.store.APITokenByHash(r.Context(), auth.HashToken(raw), now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.tok.failures.allow(ip)
		a.audit(r, "anonymous", "token.rejected", auth.APITokenHint(raw), "unknown or revoked token")
		writeError(w, http.StatusUnauthorized, "This API token is not valid. It may have been revoked.")
		return nil, false
	case errors.Is(err, store.ErrTokenExpired):
		a.tok.failures.allow(ip)
		a.audit(r, u.Email, "token.rejected", t.Name, "expired on "+t.ExpiresAt.UTC().Format("2 Jan 2006"))
		writeError(w, http.StatusUnauthorized, "This API token expired on "+t.ExpiresAt.UTC().Format("2 Jan 2006")+". Create a new one under Account › API tokens.")
		return nil, false
	case err != nil:
		a.internalError(w, r, err)
		return nil, false
	}
	if !a.tok.requests.allow(t.ID) {
		writeError(w, http.StatusTooManyRequests, "Too many requests with this token. Slow down and try again in a minute.")
		return nil, false
	}
	if now.Sub(t.LastUsedAt) >= tokenTouchEvery || t.LastUsedIP != ip {
		if err := a.store.TokenUsed(r.Context(), t.ID, now, ip); err != nil {
			a.cfg.Logger.Error("could not record token use", "err", err)
		}
	}
	eff := *u
	eff.Role = effectiveRole(t.Role, u.Role)
	return &principal{user: &eff, token: t}, true
}

// tokenDeniedPrefixes are routes a token may never use, whatever its role:
// identity and security administration, which needs a person with a browser
// session (and often their password).
var tokenDeniedPrefixes = []string{
	"/api/v1/account", "/api/v1/session", "/api/v1/members", "/api/v1/invites",
	"/api/v1/settings/sso", "/api/v1/settings/data-key",
}

// tokenProjectless are the routes without a project that a project-restricted
// token may use: its identity, and the lists and searches across projects,
// which projectScope narrows to the token's projects.
var tokenProjectless = []string{
	"GET /api/v1/session", "GET /api/v1/roles",
	"GET /api/v1/projects", "GET /api/v1/apps", "GET /api/v1/tasks", "GET /api/v1/volumes",
	"GET /api/v1/schedules", "GET /api/v1/domains",
	"GET /api/v1/metrics/overview", "GET /api/v1/metrics/query",
	"GET /api/v1/logs", "GET /api/v1/logs/tail",
	"GET /api/v1/alerts", "GET /api/v1/alerts/silences", "GET /api/v1/alerts/rules",
	"GET /api/v1/recordings", "GET /api/v1/traffic/drops", "GET /api/v1/cluster-status",
}

// tokenMayUse applies the token rules above to the matched route. It answers
// the request itself (403) when they refuse.
func (a *api) tokenMayUse(w http.ResponseWriter, r *http.Request, p *principal) bool {
	pattern := r.Pattern
	_, path, _ := strings.Cut(pattern, " ")
	deny := func(reason, msg string) bool {
		a.audit(r, p.user.Email, "token.denied", r.Method+" "+r.URL.Path, p.token.Name+": "+reason)
		writeError(w, http.StatusForbidden, msg)
		return false
	}
	if pattern != "GET /api/v1/session" {
		for _, prefix := range tokenDeniedPrefixes {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return deny("identity route", "API tokens cannot manage accounts, tokens, members or sign-in settings. Use the console.")
			}
		}
	}
	if strings.HasSuffix(path, "/shell") {
		return deny("shell", "Shells need a browser session (they are recorded). Use the console.")
	}
	if strings.HasSuffix(path, "/reveal") {
		return deny("reveal", "API tokens never reveal secret values. Use the console.")
	}
	if p.token.Projects == nil {
		return true
	}
	if project := r.PathValue("project"); project != "" {
		if slices.Contains(p.token.Projects, project) {
			return true
		}
		return deny("project "+project+" outside the token's scope", "This token is limited to the projects "+strings.Join(p.token.Projects, ", ")+".")
	}
	if slices.Contains(tokenProjectless, pattern) {
		return true
	}
	return deny("route outside a project", "This token is limited to the projects "+strings.Join(p.token.Projects, ", ")+
		"; it can only use routes of those projects.")
}

// ---- managing tokens (Account › API tokens) ------------------------------------

func (a *api) registerTokens(mux *http.ServeMux) {
	user := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(a.requireUser(h)) }
	mux.HandleFunc("GET /api/v1/account/tokens", a.requireUser(a.tokensList))
	mux.HandleFunc("POST /api/v1/account/tokens", user(a.tokenCreate))
	mux.HandleFunc("DELETE /api/v1/account/tokens/{id}", user(a.tokenRevoke))
	mux.HandleFunc("POST /api/v1/account/kubeconfig", user(a.kubeconfigCreate))
}

func tokenJSON(t *store.APIToken, now time.Time) map[string]any {
	projects := t.Projects
	if projects == nil {
		projects = []string{}
	}
	var used any
	if !t.LastUsedAt.IsZero() {
		used = t.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id": t.ID, "name": t.Name, "kind": t.Kind, "hint": t.Hint, "role": t.Role, "projects": projects,
		"createdAt": t.CreatedAt.UTC().Format(time.RFC3339), "expiresAt": t.ExpiresAt.UTC().Format(time.RFC3339),
		"lastUsedAt": used, "lastUsedIp": t.LastUsedIP, "expired": !now.Before(t.ExpiresAt),
	}
}

func (a *api) tokensList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	list, err := a.store.APITokens(r.Context(), p.user.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	now := a.now()
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, tokenJSON(&list[i], now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "maxDays": tokenMaxDays, "defaultDays": int(tokenDefaultTTL / (24 * time.Hour))})
}

type tokenRequest struct {
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	Projects      []string `json:"projects"`
	ExpiresInDays int      `json:"expiresInDays"`
}

var projectNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// newToken validates a request and stores the token. It answers the request
// itself on failure.
func (a *api) newToken(w http.ResponseWriter, r *http.Request, req tokenRequest, kind string) (string, *store.APIToken, bool) {
	p := principalOf(r)
	u := p.user
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeFieldError(w, "name", "Name the token after what uses it, like \"GitHub Actions deploy\".")
		return "", nil, false
	}
	if utf8.RuneCountInString(name) > maxTokenNameLen {
		writeFieldError(w, "name", fmt.Sprintf("Use at most %d characters.", maxTokenNameLen))
		return "", nil, false
	}
	role := req.Role
	if role == "" {
		role = u.Role
	}
	if roleRank(role) == 99 {
		writeFieldError(w, "role", "Choose one of owner, admin, developer or viewer.")
		return "", nil, false
	}
	if roleRank(role) < roleRank(u.Role) {
		writeFieldError(w, "role", "A token cannot have more power than your own role ("+u.Role+").")
		return "", nil, false
	}
	var projects []string
	for _, pr := range req.Projects {
		pr = strings.TrimSpace(pr)
		if pr == "" || slices.Contains(projects, pr) {
			continue
		}
		if len(pr) > 63 || !projectNameRE.MatchString(pr) {
			writeFieldError(w, "projects", fmt.Sprintf("%q is not a project name.", pr))
			return "", nil, false
		}
		projects = append(projects, pr)
	}
	if len(projects) > maxTokenScope {
		writeFieldError(w, "projects", fmt.Sprintf("Name at most %d projects, or none for all of them.", maxTokenScope))
		return "", nil, false
	}
	if projects != nil && roleRank(role) < roleRank(store.RoleDeveloper) {
		writeFieldError(w, "role", "A token limited to projects can be developer or viewer: owner and admin rights are not per project.")
		return "", nil, false
	}
	days := req.ExpiresInDays
	ttl := tokenDefaultTTL
	if days != 0 {
		if days < 1 || days > tokenMaxDays {
			writeFieldError(w, "expiresInDays", fmt.Sprintf("Choose between 1 and %d days.", tokenMaxDays))
			return "", nil, false
		}
		ttl = time.Duration(days) * 24 * time.Hour
	}
	raw, now := auth.NewAPIToken(), a.now()
	t := &store.APIToken{UserID: u.ID, Name: name, Kind: kind, TokenHash: auth.HashToken(raw), Hint: auth.APITokenHint(raw),
		Role: role, Projects: projects, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := a.store.CreateAPIToken(r.Context(), t); err != nil {
		if errors.Is(err, store.ErrTooManyTokens) {
			writeError(w, http.StatusConflict, fmt.Sprintf("You have %d active tokens, the most allowed. Revoke some first.", store.MaxTokensPerUser))
			return "", nil, false
		}
		a.internalError(w, r, err)
		return "", nil, false
	}
	scope := "all projects"
	if projects != nil {
		scope = "projects " + strings.Join(projects, ", ")
	}
	a.audit(r, u.Email, "token.created", name, fmt.Sprintf("%s, %s, %s, expires %s", kind, role, scope, t.ExpiresAt.UTC().Format("2 Jan 2006")))
	return raw, t, true
}

func (a *api) tokenCreate(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if !decode(w, r, &req) {
		return
	}
	raw, t, ok := a.newToken(w, r, req, store.TokenKindAPI)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": raw, "apiToken": tokenJSON(t, a.now())})
}

func (a *api) tokenRevoke(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	t, err := a.store.DeleteAPIToken(r.Context(), p.user.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "That token no longer exists. Reload the page.")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, p.user.Email, "token.revoked", t.Name, t.Kind+", "+t.Hint)
	w.WriteHeader(http.StatusNoContent)
}

// ---- kubeconfig ------------------------------------------------------------------

// publicBase is the console's own URL for links that leave the browser
// (kubeconfig, the single sign-on redirect): its configured hostname, so a
// forged Host header cannot end up in them, or the request's host in
// development.
func (a *api) publicBase(r *http.Request) string {
	if d := a.consoleDomain(); d != "" && d != "localhost" {
		return "https://" + d
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// kubeconfigCreate makes a token for kubectl and returns a kubeconfig that
// points at the console's Kubernetes proxy (kubeproxy.go) with it.
func (a *api) kubeconfigCreate(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Kube == nil {
		writeError(w, http.StatusServiceUnavailable, "This console is not connected to a Kubernetes cluster.")
		return
	}
	var req tokenRequest
	if !decode(w, r, &req) {
		return
	}
	now := a.now()
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "kubeconfig " + now.UTC().Format("2006-01-02")
	}
	raw, t, ok := a.newToken(w, r, req, store.TokenKindKubeconfig)
	if !ok {
		return
	}
	var others []string // the other clusters, each a context of its own
	for _, st := range a.clusters.states() {
		if st.name != clusters.Local {
			others = append(others, st.name)
		}
	}
	cfg, name := kubeconfigYAML(a.publicBase(r), a.consoleDomain(), principalOf(r).user.Email, raw, t.Projects, others...)
	writeJSON(w, http.StatusCreated, map[string]any{"kubeconfig": cfg, "filename": name, "apiToken": tokenJSON(t, now)})
}

// kubeconfigYAML renders a kubeconfig for the proxy at base+"/k8s". The
// cluster is named after the console's hostname, so several consoles merge
// into one kubeconfig without clashing. Every other cluster the console
// manages gets a cluster and context "kwerft-<domain>-<name>" at
// base+"/k8s/clusters/<name>" with the same token; the management cluster's
// stays the current context.
func kubeconfigYAML(base, domain, email, token string, projects []string, others ...string) (string, string) {
	if domain == "" {
		domain = "kwerft"
	}
	cluster := "kwerft-" + domain
	userName := email + "@" + cluster
	ctx := map[string]any{"cluster": cluster, "user": userName}
	if len(projects) > 0 {
		ctx["namespace"] = projects[0] // a project's namespace has its name
	}
	clustersOut := []any{map[string]any{"name": cluster, "cluster": map[string]any{"server": base + "/k8s"}}}
	contexts := []any{map[string]any{"name": cluster, "context": ctx}}
	for _, o := range others {
		name := cluster + "-" + o
		clustersOut = append(clustersOut, map[string]any{"name": name, "cluster": map[string]any{"server": base + "/k8s/clusters/" + o}})
		contexts = append(contexts, map[string]any{"name": name, "context": map[string]any{"cluster": name, "user": userName}})
	}
	cfg := map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"current-context": cluster,
		"clusters":        clustersOut,
		"users":           []any{map[string]any{"name": userName, "user": map[string]any{"token": token}}},
		"contexts":        contexts,
	}
	b, _ := yaml.Marshal(cfg)
	return "# Kwerft console " + domain + ": kubectl through the console, as " + email + ".\n" +
		"# Revoke the token under Account › API tokens.\n" + string(b), "kubeconfig-" + domain + ".yaml"
}
