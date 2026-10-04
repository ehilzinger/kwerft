package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

const (
	pendingTTL         = 5 * time.Minute // password accepted, waiting for the second factor
	ceremonyTTL        = 5 * time.Minute // a passkey prompt the browser is showing
	maxPendingFailures = 5               // wrong second factors before the password is asked again
)

// Second-factor methods, in the order the sign-in page offers them.
const (
	methodPasskey  = "passkey"
	methodTOTP     = "totp"
	methodRecovery = "recovery"
)

// mfa is the second-factor state that lives in memory: sign-ins waiting for
// their second factor and passkey ceremonies in progress. v1 runs a single
// console replica, so this is enough; a restart only means signing in again.
type mfa struct {
	sealer   *auth.Sealer // nil without a data key: TOTP is unavailable
	issuer   string       // shown in authenticator apps
	cookies  struct{ pending, passkey string }
	attempts *limiter // second-factor attempts per account
	confirm  *limiter // password or code confirmations per account

	// The relying party follows the console's hostname; see passkeyRP.
	rpMu     sync.Mutex
	webauthn *webauthn.WebAuthn // nil without a console domain: passkeys are unavailable
	rpID     string

	mu         sync.Mutex
	pending    map[string]*pendingLogin // pending cookie hash →
	ceremonies map[string]*ceremony     // purpose:key →
}

type pendingLogin struct {
	userID    string
	expires   time.Time
	failures  int
	assertion *webauthn.SessionData // set when the passkey prompt starts
}

type ceremony struct {
	data    webauthn.SessionData
	expires time.Time
}

func newMFA(cfg Config, now func() time.Time) *mfa {
	m := &mfa{
		issuer:     "Kwerft",
		attempts:   newLimiter(10, 15*time.Minute, now),
		confirm:    newLimiter(10, 15*time.Minute, now),
		pending:    map[string]*pendingLogin{},
		ceremonies: map[string]*ceremony{},
	}
	m.cookies.pending, m.cookies.passkey = "kwerft_pending", "kwerft_passkey"
	if !cfg.InsecureCookies {
		m.cookies.pending, m.cookies.passkey = "__Host-kwerft_pending", "__Host-kwerft_passkey"
	}
	if d := cfg.ConsoleDomain; d != "" && d != "localhost" {
		m.issuer = "Kwerft " + d // tells several consoles apart in the app
	}
	if len(cfg.DataKey) > 0 {
		s, err := auth.NewSealer(cfg.DataKey)
		if err != nil {
			cfg.Logger.Error("data key unusable: authenticator apps are off", "err", err)
		}
		m.sealer = s
	}
	if cfg.ConsoleDomain != "" {
		wa, err := newWebAuthn(cfg, cfg.ConsoleDomain)
		if err != nil {
			cfg.Logger.Error("passkeys are off", "err", err)
		}
		m.webauthn, m.rpID = wa, cfg.ConsoleDomain
	}
	return m
}

// consoleDomain is the hostname the console is served on now: Settings can
// move it (ConsoleSettings.status), the flag is the fallback.
func (a *api) consoleDomain() string {
	if a.cfg.ActiveConsoleDomain != nil {
		if d := a.cfg.ActiveConsoleDomain(); d != "" {
			return d
		}
	}
	return a.cfg.ConsoleDomain
}

// passkeyRP is the WebAuthn relying party for the console's current
// hostname; nil when passkeys are unavailable. Passkeys are bound to the
// hostname they were made on, so after the console moves only new ones work.
func (a *api) passkeyRP() *webauthn.WebAuthn {
	m, domain := a.mfa, a.consoleDomain()
	m.rpMu.Lock()
	defer m.rpMu.Unlock()
	if domain == "" || domain == m.rpID {
		return m.webauthn
	}
	wa, err := newWebAuthn(a.cfg, domain)
	if err != nil {
		a.cfg.Logger.Error("passkeys are off", "domain", domain, "err", err)
		return m.webauthn
	}
	m.webauthn, m.rpID = wa, domain
	return wa
}

// prune drops expired entries once the maps grow, so memory stays bounded.
// Callers hold m.mu.
func (m *mfa) prune(now time.Time) {
	if len(m.pending)+len(m.ceremonies) < 1000 {
		return
	}
	for k, p := range m.pending {
		if !now.Before(p.expires) {
			delete(m.pending, k)
		}
	}
	for k, c := range m.ceremonies {
		if !now.Before(c.expires) {
			delete(m.ceremonies, k)
		}
	}
}

func (m *mfa) putCeremony(key string, data *webauthn.SessionData, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune(now)
	m.ceremonies[key] = &ceremony{data: *data, expires: now.Add(ceremonyTTL)}
}

// takeCeremony returns and forgets a ceremony: each one is single use.
func (m *mfa) takeCeremony(key string, now time.Time) *ceremony {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.ceremonies[key]
	delete(m.ceremonies, key)
	if !ok || !now.Before(c.expires) {
		return nil
	}
	return c
}

// dropPendingFor forgets a user's half-finished sign-ins, e.g. after their
// password changed.
func (m *mfa) dropPendingFor(userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, p := range m.pending {
		if p.userID == userID {
			delete(m.pending, k)
		}
	}
}

func (a *api) registerMFA(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/session/second-factor/totp", a.sameOrigin(a.loginTOTP))
	mux.HandleFunc("POST /api/v1/session/second-factor/recovery", a.sameOrigin(a.loginRecovery))
	mux.HandleFunc("POST /api/v1/session/second-factor/passkey/begin", a.sameOrigin(a.loginPasskeyBegin))
	mux.HandleFunc("POST /api/v1/session/second-factor/passkey/finish", a.sameOrigin(a.loginPasskeyFinish))
	mux.HandleFunc("POST /api/v1/session/passkey/begin", a.sameOrigin(a.passkeyLoginBegin))
	mux.HandleFunc("POST /api/v1/session/passkey/finish", a.sameOrigin(a.passkeyLoginFinish))
	a.registerAccount(mux)
}

// ---- password accepted: ask for the second factor --------------------------

// secondFactorRequired runs once the password is right. If the user has a
// second factor it parks the sign-in behind a short-lived cookie, tells the UI
// which methods to offer and reports true; no session exists yet.
func (a *api) secondFactorRequired(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	f, err := a.store.Factors(r.Context(), u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return true
	}
	if !f.Any() {
		return false
	}
	methods := []string{}
	if f.Passkeys > 0 {
		methods = append(methods, methodPasskey)
	}
	if f.TOTP {
		methods = append(methods, methodTOTP)
	}
	if f.RecoveryCodes > 0 {
		methods = append(methods, methodRecovery)
	}
	token, now := auth.NewToken(), a.now()
	a.mfa.mu.Lock()
	a.mfa.prune(now)
	a.mfa.pending[auth.HashToken(token)] = &pendingLogin{userID: u.ID, expires: now.Add(pendingTTL)}
	a.mfa.mu.Unlock()
	a.setCookie(w, a.mfa.cookies.pending, token, now.Add(pendingTTL))
	writeJSON(w, http.StatusOK, map[string]any{"secondFactor": methods})
	return true
}

// pendingFor resolves the pending cookie to the user who entered a correct
// password. It answers the request itself when there is none.
func (a *api) pendingFor(w http.ResponseWriter, r *http.Request) (hash string, p pendingLogin, u *store.User, ok bool) {
	const timedOut = "Your sign-in timed out. Enter your password again."
	c, err := r.Cookie(a.mfa.cookies.pending)
	if err != nil {
		writeError(w, http.StatusUnauthorized, timedOut)
		return "", p, nil, false
	}
	hash = auth.HashToken(c.Value)
	a.mfa.mu.Lock()
	found := a.mfa.pending[hash]
	if found != nil && !a.now().Before(found.expires) {
		delete(a.mfa.pending, hash)
		found = nil
	}
	if found != nil {
		p = *found
	}
	a.mfa.mu.Unlock()
	if found == nil {
		a.clearCookie(w, a.mfa.cookies.pending)
		writeError(w, http.StatusUnauthorized, timedOut)
		return "", p, nil, false
	}
	u, err = a.store.UserByID(r.Context(), p.userID)
	if err != nil {
		a.internalError(w, r, err)
		return "", p, nil, false
	}
	return hash, p, u, true
}

// allowAttempt applies the per-account limit on second-factor guesses.
func (a *api) allowAttempt(w http.ResponseWriter, u *store.User) bool {
	if !a.mfa.attempts.allow(u.ID) {
		writeError(w, http.StatusTooManyRequests, "Too many attempts. Wait 15 minutes and try again.")
		return false
	}
	return true
}

// secondFactorFailed counts a wrong second factor. After a few the pending
// sign-in is dropped, so guessing further needs the password again.
func (a *api) secondFactorFailed(w http.ResponseWriter, r *http.Request, hash string, u *store.User, method, msg string) {
	a.audit(r, u.Email, "session.second_factor_failed", u.Email, method)
	a.mfa.mu.Lock()
	p := a.mfa.pending[hash]
	exhausted := false
	if p != nil {
		p.failures++
		p.assertion = nil
		if p.failures >= maxPendingFailures {
			delete(a.mfa.pending, hash)
			exhausted = true
		}
	}
	a.mfa.mu.Unlock()
	if exhausted {
		a.clearCookie(w, a.mfa.cookies.pending)
		writeError(w, http.StatusUnauthorized, "Too many wrong attempts. Enter your password again.")
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg, "field": "code"})
}

// finishLogin turns a pending sign-in into a session.
func (a *api) finishLogin(w http.ResponseWriter, r *http.Request, hash string, u *store.User, how string) {
	a.mfa.mu.Lock()
	delete(a.mfa.pending, hash)
	a.mfa.mu.Unlock()
	a.clearCookie(w, a.mfa.cookies.pending)
	if err := a.startSession(w, r, u); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, u.Email, "session.login", u.Email, how)
	writeJSON(w, http.StatusOK, userJSON(u))
}

func (a *api) loginTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	hash, _, u, ok := a.pendingFor(w, r)
	if !ok || !a.allowAttempt(w, u) {
		return
	}
	valid, reused, err := a.checkTOTP(r.Context(), u.ID, req.Code)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !valid {
		msg := "That code is not valid. Enter the current 6-digit code from your authenticator app."
		if reused {
			msg = "That code was already used. Wait for the next one and enter it."
		}
		a.secondFactorFailed(w, r, hash, u, methodTOTP, msg)
		return
	}
	a.finishLogin(w, r, hash, u, "password + authenticator app")
}

func (a *api) loginRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	hash, _, u, ok := a.pendingFor(w, r)
	if !ok || !a.allowAttempt(w, u) {
		return
	}
	ctx := r.Context()
	used, err := a.store.UseRecoveryCode(ctx, u.ID, auth.HashRecoveryCode(req.Code), a.now())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !used {
		a.secondFactorFailed(w, r, hash, u, methodRecovery,
			"That recovery code is not valid or was already used. Each code works once.")
		return
	}
	f, err := a.store.Factors(ctx, u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, u.Email, "account.recovery_code_used", u.Email, fmt.Sprintf("%d left", f.RecoveryCodes))
	a.finishLogin(w, r, hash, u, "password + recovery code")
}

// checkTOTP verifies a code from the user's authenticator app and uses up its
// time step. reused reports a correct code whose step was already used.
func (a *api) checkTOTP(ctx context.Context, userID, code string) (ok, reused bool, err error) {
	if a.mfa.sealer == nil {
		return false, false, nil
	}
	t, err := a.store.TOTPFor(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, false, nil
	}
	if err != nil || !t.Confirmed {
		return false, false, err
	}
	secret, err := a.mfa.sealer.Open(t.Secret, totpContext(userID))
	if err != nil {
		return false, false, fmt.Errorf("TOTP secret of user %s: %w (was KWERFT_DATA_KEY changed?)", userID, err)
	}
	step, match := auth.MatchTOTP(secret, code, a.now())
	if !match {
		return false, false, nil
	}
	used, err := a.store.UseTOTPStep(ctx, userID, step)
	return used, !used && err == nil, err
}

// totpContext binds a sealed TOTP secret to its user.
func totpContext(userID string) string { return "totp:" + userID }

// confirmIdentity asks for proof beyond the session cookie before changes to
// how an account signs in: the password or, if set up, a current code from
// the authenticator app. It answers the request itself on failure.
func (a *api) confirmIdentity(w http.ResponseWriter, r *http.Request, u *store.User, secret string) bool {
	if strings.TrimSpace(secret) == "" {
		writeFieldError(w, "password", "Enter your password to confirm this change.")
		return false
	}
	if !a.mfa.confirm.allow(u.ID) {
		writeError(w, http.StatusTooManyRequests, "Too many attempts. Wait 15 minutes and try again.")
		return false
	}
	if auth.LooksLikeTOTP(secret) {
		ok, _, err := a.checkTOTP(r.Context(), u.ID, secret)
		if err != nil {
			a.internalError(w, r, err)
			return false
		}
		if ok {
			return true
		}
		a.audit(r, u.Email, "account.confirm_failed", u.Email, "authenticator code")
		writeFieldError(w, "password", "That code is not valid. Enter your password or the current code from your authenticator app.")
		return false
	}
	if ok, err := auth.VerifyPassword(u.PasswordHash, secret); err == nil && ok {
		return true
	}
	a.audit(r, u.Email, "account.confirm_failed", u.Email, "password")
	writeFieldError(w, "password", "That password is not correct.")
	return false
}
