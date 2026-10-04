package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

// sessionIDLen is how much of a session's hash the account page shows as its
// ID: enough to tell sessions apart, useless for signing in.
const sessionIDLen = 16

func (a *api) registerAccount(mux *http.ServeMux) {
	user := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(a.requireUser(h)) }
	mux.HandleFunc("GET /api/v1/account", a.requireUser(a.accountGet))
	mux.HandleFunc("PATCH /api/v1/account", user(a.accountRename))
	mux.HandleFunc("POST /api/v1/account/password", user(a.accountPassword))
	mux.HandleFunc("POST /api/v1/account/totp", user(a.totpStart))
	mux.HandleFunc("POST /api/v1/account/totp/confirm", user(a.totpConfirm))
	mux.HandleFunc("POST /api/v1/account/totp/disable", user(a.totpDisable))
	mux.HandleFunc("POST /api/v1/account/recovery-codes", user(a.recoveryRegenerate))
	mux.HandleFunc("POST /api/v1/account/passkeys/begin", user(a.passkeyRegisterBegin))
	mux.HandleFunc("POST /api/v1/account/passkeys/finish", user(a.passkeyRegisterFinish))
	mux.HandleFunc("PATCH /api/v1/account/passkeys/{id}", user(a.passkeyRename))
	mux.HandleFunc("DELETE /api/v1/account/passkeys/{id}", user(a.passkeyDelete))
	mux.HandleFunc("DELETE /api/v1/account/sessions", user(a.sessionsRevokeOthers))
	mux.HandleFunc("DELETE /api/v1/account/sessions/{id}", user(a.sessionRevoke))
}

func (a *api) accountGet(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxKey{}).(*principal)
	ctx, u := r.Context(), p.user
	f, err := a.store.Factors(ctx, u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	keys, err := a.store.Passkeys(ctx, u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	passkeys := make([]map[string]any, 0, len(keys))
	for i := range keys {
		passkeys = append(passkeys, passkeyJSON(&keys[i]))
	}
	list, err := a.store.UserSessions(ctx, u.ID, a.now())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	sessions := make([]map[string]any, 0, len(list))
	for _, s := range list {
		sessions = append(sessions, map[string]any{
			"id": s.IDHash[:sessionIDLen], "current": s.IDHash == p.idHash,
			"createdAt": s.CreatedAt.UTC().Format(time.RFC3339), "lastSeenAt": s.LastSeenAt.UTC().Format(time.RFC3339),
			"ip": s.IP, "userAgent": s.UserAgent,
		})
	}
	identities, err := a.identitiesJSON(ctx, u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":              userJSON(u),
		"hasPassword":       u.PasswordHash != "",
		"identities":        identities,
		"totp":              f.TOTP,
		"passkeys":          passkeys,
		"recoveryCodesLeft": f.RecoveryCodes,
		"sessions":          sessions,
		// What this console can offer; the page explains what's missing.
		"available": map[string]bool{"totp": a.mfa.sealer != nil, "passkeys": a.passkeyRP() != nil},
	})
}

func (a *api) accountRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeFieldError(w, "name", "Enter your name.")
		return
	}
	if utf8.RuneCountInString(name) > 100 {
		writeFieldError(w, "name", "Use at most 100 characters for your name.")
		return
	}
	if err := a.store.SetUserName(r.Context(), p.user.ID, name); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, p.user.Email, "account.name_changed", p.user.Email, "")
	u := *p.user
	u.Name = name
	writeJSON(w, http.StatusOK, userJSON(&u))
}

func (a *api) accountPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	ctx, u := r.Context(), p.user
	if !a.mfa.confirm.allow(u.ID) {
		writeError(w, http.StatusTooManyRequests, "Too many attempts. Wait 15 minutes and try again.")
		return
	}
	if u.PasswordHash == "" {
		// A single sign-on account sets its first password: a recent
		// sign-in stands in for the current one.
		if !a.freshSession(r) {
			writeFieldError(w, "current", "Sign out and in again with single sign-on, then set a password within 10 minutes.")
			return
		}
	} else if ok, err := auth.VerifyPassword(u.PasswordHash, req.Current); err != nil || !ok {
		a.audit(r, u.Email, "account.confirm_failed", u.Email, "password")
		writeFieldError(w, "current", "That is not your current password.")
		return
	}
	if err := auth.CheckPassword(req.New); err != nil {
		writeFieldError(w, "new", "New password too short: "+err.Error()+".")
		return
	}
	hash, err := auth.HashPassword(req.New)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.store.SetPasswordHash(ctx, u.ID, hash); err != nil {
		a.internalError(w, r, err)
		return
	}
	// Whoever knew the old password must not stay signed in elsewhere.
	n, err := a.store.DeleteOtherSessions(ctx, u.ID, p.idHash)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.mfa.dropPendingFor(u.ID)
	a.audit(r, u.Email, "account.password_changed", u.Email, fmt.Sprintf("%d other sessions signed out", n))
	writeJSON(w, http.StatusOK, map[string]int64{"signedOut": n})
}

// ---- authenticator app (TOTP) ----------------------------------------------

func (a *api) totpReady(w http.ResponseWriter) bool {
	if a.mfa.sealer == nil {
		writeError(w, http.StatusServiceUnavailable,
			"Authenticator apps need a data key. Set KWERFT_DATA_KEY for the console (the Helm chart does this) and restart it.")
		return false
	}
	return true
}

// totpStart creates a secret and shows it; it only counts once confirmed.
func (a *api) totpStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	if !a.totpReady(w) || !a.confirmIdentity(w, r, p.user, req.Password) {
		return
	}
	secret := auth.NewTOTPSecret()
	err := a.store.StartTOTP(r.Context(), p.user.ID, a.mfa.sealer.Seal(secret, totpContext(p.user.ID)), a.now())
	if errors.Is(err, store.ErrTOTPEnabled) {
		writeError(w, http.StatusConflict, "An authenticator app is already set up. Turn it off first to set up a new one.")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	uri := auth.TOTPURI(a.mfa.issuer, p.user.Email, secret)
	qr, err := qrDataURI(uri)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": auth.TOTPSecretText(secret), "uri": uri, "qr": qr})
}

func (a *api) totpConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	ctx, u := r.Context(), p.user
	if !a.totpReady(w) || !a.allowAttempt(w, u) {
		return
	}
	t, err := a.store.TOTPFor(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.Confirmed) {
		writeError(w, http.StatusConflict, "There is no authenticator app setup in progress. Start again.")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	secret, err := a.mfa.sealer.Open(t.Secret, totpContext(u.ID))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	step, ok := auth.MatchTOTP(secret, req.Code, a.now())
	if !ok {
		writeFieldError(w, "code", "That code doesn't match. Make sure your phone sets its clock automatically, then enter the current code.")
		return
	}
	before, err := a.store.Factors(ctx, u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if ok, err := a.store.ConfirmTOTP(ctx, u.ID, step); err != nil {
		a.internalError(w, r, err)
		return
	} else if !ok {
		writeError(w, http.StatusConflict, "There is no authenticator app setup in progress. Start again.")
		return
	}
	a.audit(r, u.Email, "account.totp_enabled", u.Email, "")
	out := map[string]any{}
	if !before.Any() {
		codes, err := a.newRecoveryCodes(r, u)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		out["recoveryCodes"] = codes
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) totpDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	ctx, u := r.Context(), p.user
	if !a.confirmIdentity(w, r, u, req.Password) {
		return
	}
	t, err := a.store.TOTPFor(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "No authenticator app is set up.")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if t.Confirmed && !a.keepsRequiredFactor(w, r, u, func(f store.Factors) bool { return f.Passkeys > 0 }) {
		return
	}
	if err := a.store.DeleteTOTP(ctx, u.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	if t.Confirmed {
		a.audit(r, u.Email, "account.totp_disabled", u.Email, "")
	}
	if err := a.factorRemoved(ctx, u.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- recovery codes ----------------------------------------------------------

// newRecoveryCodes replaces the user's recovery codes and returns the new ones
// in plain text, the only time they are ever shown.
func (a *api) newRecoveryCodes(r *http.Request, u *store.User) ([]string, error) {
	codes := auth.NewRecoveryCodes(auth.RecoveryCodeCount)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = auth.HashRecoveryCode(c)
	}
	if err := a.store.ReplaceRecoveryCodes(r.Context(), u.ID, hashes); err != nil {
		return nil, err
	}
	a.audit(r, u.Email, "account.recovery_codes_generated", u.Email, "")
	return codes, nil
}

func (a *api) recoveryRegenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	if !a.confirmIdentity(w, r, p.user, req.Password) {
		return
	}
	f, err := a.store.Factors(r.Context(), p.user.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !f.Any() {
		writeError(w, http.StatusConflict, "Recovery codes stand in for a lost second factor. Set up an authenticator app or a passkey first.")
		return
	}
	codes, err := a.newRecoveryCodes(r, p.user)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// factorRemoved deletes the recovery codes once no second factor is left:
// on their own they would be a weaker second factor nobody chose.
func (a *api) factorRemoved(ctx context.Context, userID string) error {
	f, err := a.store.Factors(ctx, userID)
	if err != nil || f.Any() {
		return err
	}
	return a.store.DeleteRecoveryCodes(ctx, userID)
}

// ---- sessions ----------------------------------------------------------------

func (a *api) sessionsRevokeOthers(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxKey{}).(*principal)
	n, err := a.store.DeleteOtherSessions(r.Context(), p.user.ID, p.idHash)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, p.user.Email, "account.sessions_revoked", p.user.Email, fmt.Sprintf("%d signed out", n))
	writeJSON(w, http.StatusOK, map[string]int64{"signedOut": n})
}

func (a *api) sessionRevoke(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(ctxKey{}).(*principal)
	id := r.PathValue("id")
	if len(id) != sessionIDLen {
		writeError(w, http.StatusNotFound, "That session has already ended. Reload the page.")
		return
	}
	if strings.HasPrefix(p.idHash, id) {
		writeError(w, http.StatusBadRequest, "That is the session you are using. Use Sign out instead.")
		return
	}
	err := a.store.DeleteUserSession(r.Context(), p.user.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "That session has already ended. Reload the page.")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, p.user.Email, "account.session_revoked", p.user.Email, id)
	w.WriteHeader(http.StatusNoContent)
}
