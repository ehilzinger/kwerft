package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ehilzinger/kwerft/internal/auth"
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
	setupLim *limiter // token guesses per IP
	loginIP  *limiter // logins per IP
	loginAcc *limiter // logins per account
	mfa      *mfa     // second factors, see api_mfa.go

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
	return &api{
		cfg: cfg, store: cfg.Store, tokens: cfg.SetupTokens, now: now, cookies: names,
		setupLim: newLimiter(10, 15*time.Minute, now),
		loginIP:  newLimiter(30, 15*time.Minute, now),
		loginAcc: newLimiter(10, 15*time.Minute, now),
		grants:   map[string]time.Time{},
		mfa:      newMFA(cfg, now),
	}
}

func (a *api) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/setup", a.setupStatus)
	mux.HandleFunc("POST /api/v1/setup/verify", a.sameOrigin(a.setupVerify))
	mux.HandleFunc("POST /api/v1/setup/owner", a.sameOrigin(a.setupOwner))

	mux.HandleFunc("GET /api/v1/session", a.requireUser(a.sessionGet))
	mux.HandleFunc("POST /api/v1/session", a.sameOrigin(a.login))
	mux.HandleFunc("DELETE /api/v1/session", a.sameOrigin(a.requireUser(a.logout)))

	a.registerMFA(mux)
	a.registerMembers(mux) // members, invites, roles, audit log

	a.registerWorkloads(mux)
	a.registerJobs(mux)
	a.registerPods(mux)
	a.registerSettings(mux)
	a.registerBuilds(mux) // Git builds: list, logs, cancel (api_builds.go)
	a.registerGit(mux)    // Git connections, checks, "Build now", webhooks
	a.registerAlerts(mux) // alerts, silences, alert rules, notification channels
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

type principal struct {
	user   *store.User
	idHash string
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
// anonymous requests with 401.
func (a *api) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &principal{user: u, idHash: idHash})))
	}
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
	writeJSON(w, http.StatusOK, userJSON(r.Context().Value(ctxKey{}).(*principal).user))
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
	ok, err := auth.VerifyPassword(u.PasswordHash, req.Password)
	if err != nil || !ok {
		a.audit(r, "anonymous", "session.login_failed", u.Email, "wrong password")
		writeError(w, http.StatusUnauthorized, wrong)
		return
	}
	if a.secondFactorRequired(w, r, u) {
		return
	}
	if err := a.startSession(w, r, u); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, u.Email, "session.login", u.Email, "")
	writeJSON(w, http.StatusOK, userJSON(u))
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
func (a *api) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

// clientIP prefers X-Real-Ip, which Traefik sets (overwriting any client
// value) for traffic it proxies. TODO(phase-4): only trust it from Traefik.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); net.ParseIP(ip) != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "The request body is not valid JSON.")
		return false
	}
	return true
}

func validEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s && len(s) <= 254 && strings.Contains(s[strings.LastIndex(s, "@"):], ".")
}

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
