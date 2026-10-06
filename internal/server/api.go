package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/logs"
	"github.com/ehilzinger/kwerft/internal/observability"
	"github.com/ehilzinger/kwerft/internal/setup"
	"github.com/ehilzinger/kwerft/internal/store"
)

const (
	sessionIdle     = 7 * 24 * time.Hour  // signed out after a week without use
	sessionAbsolute = 30 * 24 * time.Hour // and after a month regardless
	touchEvery      = 5 * time.Minute
	setupGrantTTL   = 30 * time.Minute
)

// api implements setup, sign-in and the authenticated endpoints.
type api struct {
	cfg      Config
	store    *store.Store
	tokens   setup.TokenSource
	now      func() time.Time
	cookies  cookieNames
	setupLim *limiter   // token guesses per IP
	loginIP  *limiter   // logins per IP
	loginAcc *limiter   // logins per account
	mfa      *mfa       // second factors, see api_mfa.go
	tok      *tokenAuth // API token rate limits, see api_tokens.go
	// vlogs reads VictoriaLogs: log search and log history (api_logsearch.go).
	vlogs *logs.Client
	// clusters reaches every managed cluster (clusters.go); baseCtx bounds
	// their background work.
	clusters *clusterSet
	baseCtx  context.Context

	mu     sync.Mutex
	grants map[string]time.Time // setup grant hash → expiry
}

type cookieNames struct{ session, setup string }

func newAPI(cfg Config) *api {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	names := cookieNames{session: "kwerft_session", setup: "kwerft_setup"}
	if !cfg.InsecureCookies {
		// __Host- cookies must be Secure, host-only and Path=/; browsers enforce it.
		names = cookieNames{session: "__Host-kwerft_session", setup: "__Host-kwerft_setup"}
	}
	a := &api{
		cfg: cfg, store: cfg.Store, tokens: cfg.SetupTokens, now: now, cookies: names,
		setupLim: newLimiter(10, 15*time.Minute, now),
		loginIP:  newLimiter(30, 15*time.Minute, now),
		loginAcc: newLimiter(10, 15*time.Minute, now),
		grants:   map[string]time.Time{},
		mfa:      newMFA(cfg, now),
		tok:      newTokenAuth(now),
		vlogs:    logs.New(cmp.Or(cfg.LogsURL, observability.LogsURL)),
	}
	a.baseCtx = cfg.BaseContext
	if a.baseCtx == nil {
		a.baseCtx = context.Background()
	}
	local := &clusterConn{name: clusters.Local, kube: cfg.Kube, cache: cfg.KubeCache, system: cfg.System,
		systemReader: cfg.SystemReader, hubble: cfg.Hubble}
	a.clusters = newClusterSet(cfg.Clusters, local, cfg.Logger, now)
	a.clusters.connect = a.connectCluster
	a.resealAtStart(context.Background())
	return a
}

func (a *api) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/setup", a.setupStatus)
	mux.HandleFunc("POST /api/v1/setup/verify", a.sameOrigin(a.setupVerify))
	mux.HandleFunc("POST /api/v1/setup/owner", a.sameOrigin(a.setupOwner))

	mux.HandleFunc("GET /api/v1/session", a.requireUser(a.sessionGet))
	mux.HandleFunc("POST /api/v1/session", a.sameOrigin(a.login))
	mux.HandleFunc("DELETE /api/v1/session", a.sameOrigin(a.requireUser(a.logout)))

	a.registerMFA(mux)
	a.registerMembers(mux)   // members, invites, roles, audit log
	a.registerSSO(mux)       // single sign-on (api_sso.go)
	a.registerDataKey(mux)   // data-key rotation (api_datakey.go)
	a.registerTokens(mux)    // API tokens and kubeconfigs (api_tokens.go)
	a.registerKubeProxy(mux) // kubectl through the console (kubeproxy.go)

	a.registerWorkloads(mux)
	a.registerProjectAccess(mux) // Team or Members, and the members (api_project_access.go)
	a.registerJobs(mux)
	a.registerSecrets(mux) // secret sets and their write-only values (api_secrets.go)
	a.registerImport(mux)  // Compose import and templates (api_import.go)
	a.registerPods(mux)
	a.registerSettings(mux)
	a.registerBuilds(mux) // Git builds: list, logs, cancel (api_builds.go)
	a.registerGit(mux)    // Git connections, checks, "Build now", webhooks
	a.registerLogSearch(mux)
	a.registerMetrics(mux)  // charts and the explorer (api_metrics.go)
	a.registerAlerts(mux)   // alerts, silences, alert rules, notification channels
	a.registerTraffic(mux)  // traffic rules, dropped connections, isolation
	a.registerFirewall(mux) // server firewall rules and their confirmation (api_firewall.go)
	a.registerBackups(mux)  // backup target, plans, backups and restores (api_backups.go)
	a.registerUpgrades(mux) // Settings › Updates: releases, policy, upgrades (api_upgrades.go)
	a.registerTopology(mux) // the Overview's infrastructure map (api_topology.go)

	mux.HandleFunc("GET /api/v1/cluster-status", a.requireUser(a.clusterStatus)) // clusters.go
	a.registerClusters(mux)                                                      // clusters, and their agents' tunnel endpoint (api_clusters.go)
	a.registerNodes(mux)                                                         // node pools, nodes, join commands and the join endpoint (api_nodes.go)
}

// ---- setup -----------------------------------------------------------------

func (a *api) setupComplete(ctx context.Context) (bool, error) {
	n, err := a.store.CountUsers(ctx)
	return n > 0, err
}

func (a *api) setupStatus(w http.ResponseWriter, r *http.Request) {
	done, err := a.setupComplete(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	// The hostname is no secret to someone already on it; the wizard shows it.
	writeJSON(w, http.StatusOK, map[string]any{"complete": done, "consoleDomain": a.consoleDomain()})
}

func (a *api) setupVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, ip := r.Context(), clientIP(r)
	if done, err := a.setupComplete(ctx); err != nil {
		a.internalError(w, r, err)
		return
	} else if done {
		writeError(w, http.StatusConflict, "Setup is already complete. Sign in instead.")
		return
	}
	if !a.setupLim.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "Too many attempts. Wait 15 minutes and try again.")
		return
	}
	hash, expires, err := a.tokens.Hash(ctx)
	switch {
	case errors.Is(err, setup.ErrNoToken):
		writeError(w, http.StatusGone, "No setup token is active. Re-run the installer on the server to create one.")
		return
	case err != nil:
		a.internalError(w, r, err)
		return
	case !a.now().Before(expires):
		writeError(w, http.StatusGone, "This setup token has expired. Re-run the installer on the server to create a new one.")
		return
	}
	if !auth.TokenMatches(req.Token, hash) {
		a.audit(r, "anonymous", "setup.token_rejected", "setup", "")
		writeError(w, http.StatusUnauthorized, "That token doesn't match. Copy it again from /etc/kwerft/setup-token on the server.")
		return
	}

	grant := auth.NewToken()
	a.mu.Lock()
	a.grants[auth.HashToken(grant)] = a.now().Add(setupGrantTTL)
	a.mu.Unlock()
	a.setCookie(w, a.cookies.setup, grant, a.now().Add(setupGrantTTL))
	a.audit(r, "anonymous", "setup.token_accepted", "setup", "")
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) hasGrant(r *http.Request) bool {
	c, err := r.Cookie(a.cookies.setup)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.grants[auth.HashToken(c.Value)]
	return ok && a.now().Before(exp)
}

func (a *api) setupOwner(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !a.hasGrant(r) {
		writeError(w, http.StatusUnauthorized, "Enter the setup token first.")
		return
	}
	req.Name, req.Email = strings.TrimSpace(req.Name), strings.TrimSpace(req.Email)
	if req.Name == "" {
		writeFieldError(w, "name", "Enter your name.")
		return
	}
	if !validEmail(req.Email) {
		writeFieldError(w, "email", "Enter a valid email address, like you@example.com.")
		return
	}
	if err := auth.CheckPassword(req.Password); err != nil {
		writeFieldError(w, "password", "Password too short: "+err.Error()+".")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	u := &store.User{Name: req.Name, Email: req.Email, PasswordHash: hash}
	ctx := r.Context()
	if err := a.store.CreateOwner(ctx, u); err != nil {
		if errors.Is(err, store.ErrSetupComplete) {
			writeError(w, http.StatusConflict, "Setup is already complete. Sign in instead.")
			return
		}
		a.internalError(w, r, err)
		return
	}
	// The owner exists, so setup is complete whatever happens next; a token
	// that fails to delete is harmless because verify now refuses.
	if err := a.tokens.Consume(ctx); err != nil {
		a.cfg.Logger.Error("could not delete the setup token", "err", err)
	}
	a.mu.Lock()
	a.grants = map[string]time.Time{}
	a.mu.Unlock()
	a.clearCookie(w, a.cookies.setup)
	a.audit(r, u.Email, "setup.owner_created", u.Email, "")

	if err := a.startSession(w, r, u); err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, userJSON(u))
}

// ---- sessions --------------------------------------------------------------

type ctxKey struct{}

// principal is who a request acts as: a session (idHash) or an API token
// (token, see api_tokens.go). For a token, user is a copy of the user whose
// Role is the token's effective role (the lower of the token's cap and the
// user's role), so role checks and impersonation honour the cap unchanged.
type principal struct {
	user   *store.User
	idHash string
	// sessionCreated is when the session's sign-in happened (zero for tokens).
	sessionCreated time.Time
	token          *store.APIToken
	// mustEnrol: the console requires a second factor and this user has
	// none; until they set one up, only enrolment endpoints answer.
	mustEnrol bool
}

// stillValid reports whether the session or token behind a long-lived stream
// still exists and acts with the same role: a stream was authorized for the
// role it started with.
func (a *api) stillValid(ctx context.Context, pr *principal) bool {
	if pr.token != nil {
		t, u, err := a.store.APITokenByHash(ctx, pr.token.TokenHash, a.now())
		return err == nil && effectiveRole(t.Role, u.Role) == pr.user.Role
	}
	_, u, err := a.store.SessionByHash(ctx, pr.idHash, a.now())
	return err == nil && u.Role == pr.user.Role
}

// enrolPatterns are the routes a user who must enrol a second factor may
// use: see who they are, set up a passkey or authenticator app (and the
// recovery codes that come with it), and sign out.
var enrolPatterns = map[string]bool{
	"GET /api/v1/session":                  true,
	"DELETE /api/v1/session":               true,
	"GET /api/v1/account":                  true,
	"POST /api/v1/account/totp":            true,
	"POST /api/v1/account/totp/confirm":    true,
	"POST /api/v1/account/recovery-codes":  true,
	"POST /api/v1/account/passkeys/begin":  true,
	"POST /api/v1/account/passkeys/finish": true,
	"GET /api/v1/sign-in-policy":           true,
}

// mustEnrol reports whether the console requires a second factor that u has
// not set up.
func (a *api) mustEnrol(ctx context.Context, u *store.User) (bool, error) {
	on, err := a.store.RequireTwoFactor(ctx)
	if err != nil || !on {
		return false, err
	}
	f, err := a.store.Factors(ctx, u.ID)
	return !f.Any(), err
}

// keepsRequiredFactor refuses to remove a user's last second factor while
// the console requires one; left says whether a factor remains after the
// removal. On false it has answered.
func (a *api) keepsRequiredFactor(w http.ResponseWriter, r *http.Request, u *store.User, left func(store.Factors) bool) bool {
	on, err := a.store.RequireTwoFactor(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return false
	}
	if !on {
		return true
	}
	f, err := a.store.Factors(r.Context(), u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return false
	}
	if !left(f) {
		writeError(w, http.StatusConflict, "This console requires a second factor. Add another passkey or an authenticator app before removing this one.")
		return false
	}
	return true
}

// sessionJSON is the signed-in user as the UI needs it after sign-in.
func sessionJSON(u *store.User, mustEnrol bool) map[string]any {
	out := map[string]any{}
	for k, v := range userJSON(u) {
		out[k] = v
	}
	if mustEnrol {
		out["mustEnrol"] = true
	}
	return out
}

func (a *api) startSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token := auth.NewToken()
	now := a.now()
	sess := &store.Session{
		IDHash: auth.HashToken(token), UserID: u.ID,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(sessionIdle),
		IP: clientIP(r), UserAgent: truncate(r.UserAgent(), 200),
	}
	if err := a.store.CreateSession(r.Context(), sess); err != nil {
		return err
	}
	a.setCookie(w, a.cookies.session, token, sess.ExpiresAt)
	return nil
}

// requireUser resolves the session cookie, slides its expiry and rejects
// anonymous requests with 401. A request with an Authorization header is
// authenticated by its API token alone, never by a cookie (api_tokens.go).
func (a *api) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hasAuthorization(r) {
			p, ok := a.tokenPrincipal(w, r)
			if !ok || !a.tokenMayUse(w, r, p) {
				return
			}
			a.serveAs(w, r, p, next)
			return
		}
		c, err := r.Cookie(a.cookies.session)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
		now := a.now()
		idHash := auth.HashToken(c.Value)
		sess, u, err := a.store.SessionByHash(r.Context(), idHash, now)
		if errors.Is(err, store.ErrNotFound) {
			a.clearCookie(w, a.cookies.session)
			writeError(w, http.StatusUnauthorized, "Your session has ended. Sign in again.")
			return
		}
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		if now.Sub(sess.LastSeenAt) > touchEvery {
			expires := now.Add(sessionIdle)
			if limit := sess.CreatedAt.Add(sessionAbsolute); expires.After(limit) {
				expires = limit
			}
			if err := a.store.TouchSession(r.Context(), idHash, now, expires); err == nil {
				a.setCookie(w, a.cookies.session, c.Value, expires)
			}
		}
		pr := &principal{user: u, idHash: idHash, sessionCreated: sess.CreatedAt}
		if pr.mustEnrol, err = a.mustEnrol(r.Context(), u); err != nil {
			a.internalError(w, r, err)
			return
		}
		if pr.mustEnrol && !enrolPatterns[r.Pattern] {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "This console requires a second factor. Set up a passkey or an authenticator app on your Account page to continue.",
				"code":  "enrolSecondFactor",
			})
			return
		}
		a.serveAs(w, r, pr, next)
	}
}

// serveAs calls next as the principal, in the cluster of the route's
// project (clusters.go).
func (a *api) serveAs(w http.ResponseWriter, r *http.Request, p *principal, next http.HandlerFunc) {
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
	r, ok := a.inProjectCluster(w, r)
	if !ok {
		return
	}
	next(w, r)
}

func (a *api) requireRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(ctxKey{}).(*principal)
		for _, role := range roles {
			if p.user.Role == role {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "Your role does not allow this.")
	}
}

func (a *api) sessionGet(w http.ResponseWriter, r *http.Request) {
	pr := principalOf(r)
	writeJSON(w, http.StatusOK, sessionJSON(pr.user, pr.mustEnrol))
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	ip, email := clientIP(r), strings.ToLower(strings.TrimSpace(req.Email))
	if !a.loginIP.allow(ip) || !a.loginAcc.allow(email) {
		writeError(w, http.StatusTooManyRequests, "Too many sign-in attempts. Wait 15 minutes and try again.")
		return
	}
	const wrong = "Email or password is incorrect."
	u, err := a.store.UserByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		auth.DummyVerify(req.Password) // same timing as a real account
		a.audit(r, "anonymous", "session.login_failed", email, "unknown account")
		writeError(w, http.StatusUnauthorized, wrong)
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if u.PasswordHash == "" {
		// Single sign-on only (api_sso.go): no password to check.
		auth.DummyVerify(req.Password)
		a.audit(r, "anonymous", "session.login_failed", u.Email, "no password (single sign-on account)")
		writeError(w, http.StatusUnauthorized, wrong)
		return
	}
	ok, err := auth.VerifyPassword(u.PasswordHash, req.Password)
	if err != nil || !ok {
		a.audit(r, "anonymous", "session.login_failed", u.Email, "wrong password")
		writeError(w, http.StatusUnauthorized, wrong)
		return
	}
	if a.secondFactorRequired(w, r, u) {
		return
	}
	// No second factor (secondFactorRequired would have asked for it). If
	// the console requires one, the session only reaches enrolment until
	// there is one (requireUser): sent to enrol, never locked out.
	enrol, err := a.mustEnrol(r.Context(), u)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.startSession(w, r, u); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, u.Email, "session.login", u.Email, map[bool]string{true: "must set up a second factor"}[enrol])
	writeJSON(w, http.StatusOK, sessionJSON(u, enrol))
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxKey{}).(*principal)
	if err := a.store.DeleteSession(r.Context(), p.idHash); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.clearCookie(w, a.cookies.session)
	a.audit(r, p.user.Email, "session.logout", p.user.Email, "")
	w.WriteHeader(http.StatusNoContent)
}

// ---- request hygiene -------------------------------------------------------

// sameOrigin rejects state-changing requests from other sites (CSRF). Browsers
// send Origin on every cross-site POST/DELETE, and Sec-Fetch-Site when they
// can't. Non-browser clients send neither and carry no cookies anyway.
//
// A request with a bearer token skips the check: a browser never attaches
// one by itself (a cross-site page would need CORS, which the console never
// grants), and requireUser then ignores cookies, so there is no ambient
// credential to forge a request with.
func (a *api) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := bearerToken(r); ok {
			next(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				writeError(w, http.StatusForbidden, "Cross-site request refused.")
				return
			}
		} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "Cross-site request refused.")
			return
		}
		next(w, r)
	}
}

func (a *api) setCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", Expires: expires,
		HttpOnly: true, Secure: !a.cfg.InsecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (a *api) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: !a.cfg.InsecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (a *api) audit(r *http.Request, actor, action, target, detail string) {
	if err := a.store.Audit(r.Context(), store.AuditEntry{
		At: a.now(), Actor: actor, Action: action, Target: target, IP: clientIP(r), Detail: detail,
	}); err != nil {
		a.cfg.Logger.Error("audit write failed", "action", action, "err", err)
	}
}

func (a *api) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.cfg.Logger.Error("request failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "Something went wrong on the server. Details are in the console logs.")
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "The request body is not valid JSON.")
		return false
	}
	return true
}

func validEmail(s string) bool { return auth.ValidEmail(s) }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func userJSON(u *store.User) map[string]string {
	return map[string]string{"id": u.ID, "email": u.Email, "name": u.Name, "role": u.Role}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeFieldError(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "field": field})
}
