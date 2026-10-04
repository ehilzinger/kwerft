package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

// maxPasskeys per account: plenty for every device someone owns, and it keeps
// the allow list of a sign-in prompt small.
const maxPasskeys = 20

// newWebAuthn configures the relying party: the console hostname, accepted
// from https://<host> unless PasskeyOrigins says otherwise (--dev).
func newWebAuthn(cfg Config, domain string) (*webauthn.WebAuthn, error) {
	origins := cfg.PasskeyOrigins
	if len(origins) == 0 {
		origins = []string{"https://" + domain}
	}
	return webauthn.New(&webauthn.Config{
		RPID:                  domain,
		RPDisplayName:         "Kwerft",
		RPOrigins:             origins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
	})
}

// passkeyUser adapts a console user to the WebAuthn library. The user handle
// is the random user ID: opaque and free of personal data, as the spec asks.
type passkeyUser struct {
	u     *store.User
	creds []webauthn.Credential
}

func (p *passkeyUser) WebAuthnID() []byte                         { return []byte(p.u.ID) }
func (p *passkeyUser) WebAuthnName() string                       { return p.u.Email }
func (p *passkeyUser) WebAuthnDisplayName() string                { return p.u.Name }
func (p *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return p.creds }

func (a *api) passkeyUser(ctx context.Context, u *store.User) (*passkeyUser, error) {
	list, err := a.store.Passkeys(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	pu := &passkeyUser{u: u}
	for _, p := range list {
		var c webauthn.Credential
		if err := json.Unmarshal(p.Credential, &c); err != nil {
			return nil, err
		}
		pu.creds = append(pu.creds, c)
	}
	return pu, nil
}

func (a *api) passkeysReady(w http.ResponseWriter) bool {
	if a.passkeyRP() == nil {
		writeError(w, http.StatusServiceUnavailable, "Passkeys are not available: the console was started without --console-domain.")
		return false
	}
	return true
}

// passkeyUsed stores the updated sign count and flags after a sign-in.
func (a *api) passkeyUsed(ctx context.Context, cred *webauthn.Credential) error {
	p, err := a.store.PasskeyByCredentialID(ctx, cred.ID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return a.store.PasskeyUsed(ctx, p.ID, b, a.now())
}

// readBody reads a WebAuthn response, which the library parses itself.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "The request body is too large.")
		return nil, false
	}
	return b, true
}

// ---- passkey as the second factor ------------------------------------------

func (a *api) loginPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	hash, _, u, ok := a.pendingFor(w, r)
	if !ok || !a.passkeysReady(w) {
		return
	}
	pu, err := a.passkeyUser(r.Context(), u)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if len(pu.creds) == 0 {
		writeError(w, http.StatusConflict, "This account has no passkeys. Use another way to confirm it's you.")
		return
	}
	opts, data, err := a.passkeyRP().BeginLogin(pu, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.mfa.mu.Lock()
	if p := a.mfa.pending[hash]; p != nil {
		p.assertion = data
	}
	a.mfa.mu.Unlock()
	writeJSON(w, http.StatusOK, opts)
}

func (a *api) loginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	hash, p, u, ok := a.pendingFor(w, r)
	if !ok || !a.passkeysReady(w) || !a.allowAttempt(w, u) {
		return
	}
	if p.assertion == nil {
		writeError(w, http.StatusConflict, "Start the passkey prompt again.")
		return
	}
	a.mfa.mu.Lock()
	if cur := a.mfa.pending[hash]; cur != nil {
		cur.assertion = nil // one response per challenge
	}
	a.mfa.mu.Unlock()
	const rejected = "That passkey was not accepted. Try again, or use another way to confirm it's you."
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		a.secondFactorFailed(w, r, hash, u, methodPasskey, rejected)
		return
	}
	pu, err := a.passkeyUser(r.Context(), u)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	cred, err := a.passkeyRP().ValidateLogin(pu, *p.assertion, parsed)
	if err != nil || cred.Authenticator.CloneWarning {
		a.secondFactorFailed(w, r, hash, u, methodPasskey, rejected)
		return
	}
	if err := a.passkeyUsed(r.Context(), cred); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.finishLogin(w, r, hash, u, "password + passkey")
}

// ---- passwordless sign-in with a discoverable passkey -----------------------

func (a *api) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !a.loginIP.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "Too many sign-in attempts. Wait 15 minutes and try again.")
		return
	}
	if !a.passkeysReady(w) {
		return
	}
	// User verification is required: the passkey stands in for the password
	// and the second factor at once.
	opts, data, err := a.passkeyRP().BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	token, now := auth.NewToken(), a.now()
	a.mfa.putCeremony("login:"+auth.HashToken(token), data, now)
	a.setCookie(w, a.mfa.cookies.passkey, token, now.Add(ceremonyTTL))
	writeJSON(w, http.StatusOK, opts)
}

func (a *api) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !a.passkeysReady(w) {
		return
	}
	var cer *ceremony
	if c, err := r.Cookie(a.mfa.cookies.passkey); err == nil {
		cer = a.mfa.takeCeremony("login:"+auth.HashToken(c.Value), a.now())
	}
	a.clearCookie(w, a.mfa.cookies.passkey)
	if cer == nil {
		writeError(w, http.StatusUnauthorized, "The passkey prompt timed out. Try again.")
		return
	}
	const rejected = "That passkey is not registered with this console. Sign in with your password, then add the passkey on your account page."
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		a.audit(r, "anonymous", "session.login_failed", "passkey", "unreadable passkey response")
		writeError(w, http.StatusUnauthorized, rejected)
		return
	}
	ctx := r.Context()
	var owner *store.User
	lookup := func(rawID, userHandle []byte) (webauthn.User, error) {
		p, err := a.store.PasskeyByCredentialID(ctx, rawID)
		if err != nil {
			return nil, err
		}
		if string(userHandle) != p.UserID {
			return nil, errors.New("user handle does not match the passkey's owner")
		}
		u, err := a.store.UserByID(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		owner = u
		return a.passkeyUser(ctx, u)
	}
	_, cred, err := a.passkeyRP().ValidatePasskeyLogin(lookup, cer.data, parsed)
	if err != nil || cred.Authenticator.CloneWarning {
		target := "passkey"
		if owner != nil {
			target = owner.Email
		}
		a.audit(r, "anonymous", "session.login_failed", target, "passkey not accepted")
		writeError(w, http.StatusUnauthorized, rejected)
		return
	}
	if err := a.passkeyUsed(ctx, cred); err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.startSession(w, r, owner); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, owner.Email, "session.login", owner.Email, "passkey")
	writeJSON(w, http.StatusOK, userJSON(owner))
}

// ---- managing passkeys on the account page ---------------------------------

func passkeyJSON(p *store.Passkey) map[string]any {
	out := map[string]any{"id": p.ID, "name": p.Name, "createdAt": p.CreatedAt.UTC().Format(time.RFC3339), "lastUsedAt": nil}
	if !p.LastUsedAt.IsZero() {
		out["lastUsedAt"] = p.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// passkeyName validates a name typed for a passkey.
func passkeyName(w http.ResponseWriter, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > 60 {
		writeFieldError(w, "name", "Use at most 60 characters for the name.")
		return "", false
	}
	return name, true
}

func (a *api) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	if !a.passkeysReady(w) || !a.confirmIdentity(w, r, p.user, req.Password) {
		return
	}
	pu, err := a.passkeyUser(r.Context(), p.user)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if len(pu.creds) >= maxPasskeys {
		writeError(w, http.StatusConflict, "This account has 20 passkeys, the most it can have. Remove one you no longer use first.")
		return
	}
	opts, data, err := a.passkeyRP().BeginRegistration(pu,
		webauthn.WithExclusions(webauthn.Credentials(pu.creds).CredentialDescriptors()),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.mfa.putCeremony("register:"+p.idHash, data, a.now())
	writeJSON(w, http.StatusOK, opts)
}

func (a *api) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	name, ok := passkeyName(w, req.Name)
	if !ok || !a.passkeysReady(w) {
		return
	}
	cer := a.mfa.takeCeremony("register:"+p.idHash, a.now())
	if cer == nil {
		writeError(w, http.StatusConflict, "Adding the passkey timed out. Start again.")
		return
	}
	ctx, u := r.Context(), p.user
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		writeError(w, http.StatusBadRequest, "The browser's passkey response could not be read. Try again.")
		return
	}
	pu, err := a.passkeyUser(ctx, u)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	cred, err := a.passkeyRP().CreateCredential(pu, cer.data, parsed)
	if err != nil {
		a.cfg.Logger.Info("passkey registration rejected", "user", u.Email, "err", err)
		writeError(w, http.StatusBadRequest, "The passkey could not be verified. Try again, or use another authenticator.")
		return
	}
	before, err := a.store.Factors(ctx, u.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	b, err := json.Marshal(cred)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	pk := &store.Passkey{UserID: u.ID, CredentialID: cred.ID, Name: name, Credential: b, CreatedAt: a.now()}
	if err := a.store.AddPasskey(ctx, pk); err != nil {
		if errors.Is(err, store.ErrPasskeyExists) {
			writeError(w, http.StatusConflict, "This passkey is already registered.")
			return
		}
		a.internalError(w, r, err)
		return
	}
	a.audit(r, u.Email, "account.passkey_added", u.Email, name)
	out := map[string]any{"passkey": passkeyJSON(pk)}
	if !before.Any() {
		codes, err := a.newRecoveryCodes(r, u)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		out["recoveryCodes"] = codes
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *api) passkeyRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := r.Context().Value(ctxKey{}).(*principal)
	name, ok := passkeyName(w, req.Name)
	if !ok {
		return
	}
	err := a.store.RenamePasskey(r.Context(), p.user.ID, r.PathValue("id"), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "That passkey no longer exists. Reload the page.")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, p.user.Email, "account.passkey_renamed", p.user.Email, name)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) passkeyDelete(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	list, err := a.store.Passkeys(ctx, p.user.ID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	name, found := "", false
	for _, pk := range list {
		if pk.ID == r.PathValue("id") {
			name, found = pk.Name, true
		}
	}
	if found && !a.keepsRequiredFactor(w, r, p.user, func(f store.Factors) bool { return f.TOTP || f.Passkeys > 1 }) {
		return
	}
	err = a.store.DeletePasskey(ctx, p.user.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "That passkey no longer exists. Reload the page.")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.audit(r, p.user.Email, "account.passkey_removed", p.user.Email, name)
	if err := a.factorRemoved(ctx, p.user.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
