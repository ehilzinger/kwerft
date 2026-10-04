package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Single sign-on through OpenID Connect (internal/auth/oidc.go does the
// protocol). The settings live in ConsoleSettings.spec.sso, written as the
// user like every setting; the client secret in the write-only Secret
// kwerft-oidc-client, which only the console's own identity reads.
//
// Who may sign in:
//   - A provider account already linked to a user (issuer + subject) signs in
//     as that user.
//   - Otherwise the verified email must belong to a user — the link is made
//     then — or to an open invite, which the sign-in accepts. A user has one
//     link per provider: another provider account that later claims the same
//     email is refused rather than linked.
//   - Otherwise, with auto-join on, a verified email in an allowed domain
//     gets a new account with the default role (developer or viewer, never
//     more). Auto-join requires allowed domains, so "anyone with a Google
//     account" can never join.
//   - Allowed domains, when set, apply to every sign-in.
//
// Second factors: a provider sign-in replaces the password, not the user's
// own second factors. Someone who set up a passkey or authenticator app in
// Kwerft is asked for it after the provider, exactly as after a password
// (the same pending sign-in, api_mfa.go), so a taken-over provider account
// alone is not enough. The "require 2FA" setting (W1) applies to provider
// sign-ins in the same place as to password sign-ins.
//
// Accounts made through single sign-on have no password. They confirm
// account changes by having signed in within the last ten minutes, and may
// set a password then.

const (
	ssoFlowTTL = 10 * time.Minute
	// freshSignIn is how recent a sign-in must be to stand in for the
	// password of an account that has none.
	freshSignIn = 10 * time.Minute
)

// ssoConfig is the provider as configured, secret included.
type ssoConfig struct {
	kwerftv1.SSOSettings
	ClientSecret string
}

type ssoFlow struct {
	flow    auth.OIDCFlow
	issuer  string
	next    string // where the browser goes afterwards (a path on the console)
	expires time.Time
}

// safeNext keeps a return path on the console: an absolute path, never
// another host ("//evil.example", "/\\evil.example").
func safeNext(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") || len(next) > 500 {
		return "/"
	}
	return next
}

type ssoAPI struct {
	*api
	oidc *auth.OIDC
	// load reads the settings and secret; nil config means not configured.
	load   func(ctx context.Context) (*ssoConfig, error)
	cookie string

	mu    sync.Mutex
	flows map[string]*ssoFlow // flow cookie hash →
}

func (a *api) registerSSO(mux *http.ServeMux) {
	s := &ssoAPI{api: a, oidc: &auth.OIDC{Now: a.now}, flows: map[string]*ssoFlow{}, cookie: "kwerft_sso"}
	if !a.cfg.InsecureCookies {
		s.cookie = "__Host-kwerft_sso"
	}
	s.load = s.loadFromCluster
	if a.cfg.ssoHook != nil {
		a.cfg.ssoHook(s)
	}

	mux.HandleFunc("GET /api/v1/sso", s.status)
	mux.HandleFunc("GET /api/v1/sso/start", s.start)
	mux.HandleFunc("GET /api/v1/sso/callback", s.callback)

	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireRole(h, store.RoleOwner, store.RoleAdmin)))
	}
	mux.HandleFunc("GET /api/v1/settings/sso", admin(s.settingsGet))
	mux.HandleFunc("PUT /api/v1/settings/sso", a.sameOrigin(admin(s.settingsPut)))
	mux.HandleFunc("POST /api/v1/account/sso/unlink", a.sameOrigin(a.requireUser(s.unlink)))
}

// loadFromCluster reads spec.sso through the informer cache and the client
// secret with the console's own identity (owners and admins cannot read it).
func (s *ssoAPI) loadFromCluster(ctx context.Context) (*ssoConfig, error) {
	if s.cfg.KubeCache == nil || s.cfg.SystemReader == nil {
		return nil, nil
	}
	var cs kwerftv1.ConsoleSettings
	err := s.cfg.KubeCache.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cs)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cs.Spec.SSO == nil {
		return nil, nil
	}
	cfg := &ssoConfig{SSOSettings: *cs.Spec.SSO}
	var sec corev1.Secret
	err = s.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.OIDCSecret}, &sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	cfg.ClientSecret = string(sec.Data[controllers.OIDCSecretKey])
	return cfg, nil
}

// active returns the configuration when single sign-on is usable.
func (s *ssoAPI) active(ctx context.Context) *ssoConfig {
	cfg, err := s.load(ctx)
	if err != nil {
		s.cfg.Logger.Error("single sign-on settings", "err", err)
		return nil
	}
	if cfg == nil || !cfg.Enabled || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Issuer == "" {
		return nil
	}
	return cfg
}

func (s *ssoAPI) redirectURL(r *http.Request) string { return s.publicBase(r) + "/api/v1/sso/callback" }

func (s *ssoAPI) oidcConfig(r *http.Request, cfg *ssoConfig) auth.OIDCConfig {
	return auth.OIDCConfig{Provider: cfg.Provider, Issuer: cfg.Issuer, ClientID: cfg.ClientID,
		ClientSecret: cfg.ClientSecret, RedirectURL: s.redirectURL(r)}
}

func displayName(cfg *kwerftv1.SSOSettings) string {
	if cfg.DisplayName != "" {
		return cfg.DisplayName
	}
	switch cfg.Provider {
	case auth.OIDCGoogle:
		return "Google"
	case auth.OIDCMicrosoft:
		return "Microsoft"
	case auth.OIDCKeycloak:
		return "Keycloak"
	}
	return "single sign-on"
}

// status tells the sign-in page whether to show the button.
func (s *ssoAPI) status(w http.ResponseWriter, r *http.Request) {
	cfg := s.active(r.Context())
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "provider": cfg.Provider, "displayName": displayName(&cfg.SSOSettings)})
}

// fail sends the browser back to the sign-in page with a reason the page
// explains (Login.tsx).
func (s *ssoAPI) fail(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?sso="+url.QueryEscape(code), http.StatusSeeOther)
}

// start begins a sign-in: a flow bound to this browser by a cookie, then off
// to the provider.
func (s *ssoAPI) start(w http.ResponseWriter, r *http.Request) {
	if !s.loginIP.allow(clientIP(r)) {
		s.fail(w, r, "limited")
		return
	}
	cfg := s.active(r.Context())
	if cfg == nil {
		s.fail(w, r, "unavailable")
		return
	}
	flow := auth.NewOIDCFlow()
	target, err := s.oidc.AuthURL(r.Context(), s.oidcConfig(r, cfg), flow)
	if err != nil {
		s.cfg.Logger.Error("single sign-on: provider unreachable", "issuer", cfg.Issuer, "err", err)
		s.fail(w, r, "provider")
		return
	}
	cookie, now := auth.NewToken(), s.now()
	s.mu.Lock()
	if len(s.flows) > 1000 {
		for k, f := range s.flows {
			if !now.Before(f.expires) {
				delete(s.flows, k)
			}
		}
	}
	s.flows[auth.HashToken(cookie)] = &ssoFlow{flow: flow, issuer: cfg.Issuer, next: safeNext(r.URL.Query().Get("next")), expires: now.Add(ssoFlowTTL)}
	s.mu.Unlock()
	// SameSite=Lax: the provider's redirect back is a top-level GET, which
	// carries it.
	s.setCookie(w, s.cookie, cookie, now.Add(ssoFlowTTL))
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// takeFlow returns and forgets the browser's flow: each one is single use.
func (s *ssoAPI) takeFlow(r *http.Request) *ssoFlow {
	c, err := r.Cookie(s.cookie)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := auth.HashToken(c.Value)
	f := s.flows[h]
	delete(s.flows, h)
	if f == nil || !s.now().Before(f.expires) {
		return nil
	}
	return f
}

func (s *ssoAPI) callback(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	f := s.takeFlow(r)
	s.clearCookie(w, s.cookie)
	q := r.URL.Query()
	switch {
	case !s.loginIP.allow(ip):
		s.fail(w, r, "limited")
		return
	case f == nil:
		s.fail(w, r, "expired")
		return
	case q.Get("state") == "" || !auth.TokenMatches(q.Get("state"), auth.HashToken(f.flow.State)):
		s.audit(r, "anonymous", "session.sso_failed", f.issuer, "state does not match")
		s.fail(w, r, "expired")
		return
	case q.Get("error") != "":
		// The user cancelled, or the provider refused (e.g. not assigned to
		// the app). Its error code is safe to record; the description is not
		// shown.
		s.audit(r, "anonymous", "session.sso_failed", f.issuer, "provider: "+truncate(q.Get("error"), 60))
		s.fail(w, r, "denied")
		return
	}
	ctx := r.Context()
	cfg := s.active(ctx)
	if cfg == nil || cfg.Issuer != f.issuer {
		s.fail(w, r, "unavailable") // turned off or changed during the sign-in
		return
	}
	id, err := s.oidc.Finish(ctx, s.oidcConfig(r, cfg), f.flow, q.Get("code"))
	switch {
	case errors.Is(err, auth.ErrOIDCUnverified), errors.Is(err, auth.ErrOIDCNoEmail):
		s.audit(r, "anonymous", "session.sso_failed", id.Email, err.Error())
		s.fail(w, r, "unverified")
		return
	case err != nil:
		s.cfg.Logger.Warn("single sign-on failed", "issuer", cfg.Issuer, "err", err)
		s.audit(r, "anonymous", "session.sso_failed", cfg.Issuer, truncate(err.Error(), 200))
		s.fail(w, r, "failed")
		return
	}
	if !domainAllowed(id.Email, cfg.AllowedDomains) {
		s.audit(r, "anonymous", "session.sso_failed", id.Email, "email domain not allowed")
		s.fail(w, r, "domain")
		return
	}
	u, code := s.resolveUser(r, cfg, id)
	if u == nil {
		s.fail(w, r, code)
		return
	}
	how := "single sign-on (" + displayName(&cfg.SSOSettings) + ")"
	methods, err := s.pendingSecondFactor(ctx, w, u, how)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(methods) > 0 {
		q := url.Values{"second-factor": {strings.Join(methods, ",")}}
		if f.next != "/" {
			q.Set("next", f.next)
		}
		http.Redirect(w, r, "/login?"+q.Encode(), http.StatusSeeOther)
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, u.Email, "session.login", u.Email, how)
	http.Redirect(w, r, f.next, http.StatusSeeOther)
}

func domainAllowed(email string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	return at > 0 && slices.Contains(domains, strings.ToLower(email[at+1:]))
}

// resolveUser finds or creates the user for a provider identity, following
// the rules at the top of this file. On refusal it returns the reason code.
func (s *ssoAPI) resolveUser(r *http.Request, cfg *ssoConfig, id *auth.OIDCIdentity) (*store.User, string) {
	ctx, now := r.Context(), s.now()
	u, err := s.store.UserByIdentity(ctx, id.Issuer, id.Subject)
	if err == nil {
		if err := s.store.IdentityUsed(ctx, id.Issuer, id.Subject, now); err != nil {
			s.cfg.Logger.Error("could not record the sign-in", "err", err)
		}
		return u, ""
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.cfg.Logger.Error("single sign-on lookup", "err", err)
		return nil, "failed"
	}
	link := store.Identity{Issuer: id.Issuer, Subject: id.Subject, Email: id.Email, CreatedAt: now}
	u, err = s.store.UserByEmail(ctx, id.Email)
	switch {
	case err == nil:
		link.UserID = u.ID
		if err := s.store.LinkIdentity(ctx, link); errors.Is(err, store.ErrIdentityConflict) {
			s.audit(r, "anonymous", "session.sso_failed", u.Email, "already linked to another account at "+id.Issuer)
			return nil, "conflict"
		} else if err != nil {
			s.cfg.Logger.Error("single sign-on link", "err", err)
			return nil, "failed"
		}
		s.audit(r, u.Email, "account.sso_linked", u.Email, id.Issuer)
		return u, ""
	case !errors.Is(err, store.ErrNotFound):
		s.cfg.Logger.Error("single sign-on lookup", "err", err)
		return nil, "failed"
	}
	name := id.Name
	if name == "" || utf8.RuneCountInString(name) > 100 {
		name = id.Email[:strings.LastIndex(id.Email, "@")]
	}
	nu := &store.User{Name: name}
	inv, err := s.store.AcceptInviteByEmail(ctx, id.Email, nu, link, now)
	switch {
	case err == nil:
		s.audit(r, nu.Email, "member.invite_accepted", nu.Email, "as "+nu.Role+" through single sign-on, invited by "+inv.InvitedByEmail)
		return nu, ""
	case !errors.Is(err, store.ErrNotFound):
		s.cfg.Logger.Error("single sign-on invite", "err", err)
		return nil, "failed"
	}
	if !cfg.AutoJoin || len(cfg.AllowedDomains) == 0 {
		s.audit(r, "anonymous", "session.sso_failed", id.Email, "not a member")
		return nil, "not_member"
	}
	role := cfg.DefaultRole
	if role != store.RoleDeveloper {
		role = store.RoleViewer
	}
	nu = &store.User{Email: id.Email, Name: name, Role: role}
	if err := s.store.CreateUserWithIdentity(ctx, nu, link, now); err != nil {
		s.cfg.Logger.Error("single sign-on auto-join", "err", err)
		return nil, "failed"
	}
	s.audit(r, nu.Email, "member.auto_joined", nu.Email, "as "+role+" through single sign-on ("+id.Issuer+")")
	return nu, ""
}

// ---- Settings › Single sign-on -------------------------------------------------

type ssoSettingsJSON struct {
	Enabled        bool     `json:"enabled"`
	Provider       string   `json:"provider"`
	Issuer         string   `json:"issuer"`
	Tenant         string   `json:"tenant,omitempty"`
	ClientID       string   `json:"clientId"`
	ClientSecret   string   `json:"clientSecret,omitempty"` // write-only: never sent back
	DisplayName    string   `json:"displayName"`
	AllowedDomains []string `json:"allowedDomains"`
	AutoJoin       bool     `json:"autoJoin"`
	DefaultRole    string   `json:"defaultRole"`
}

func (s *ssoAPI) settingsView(r *http.Request, cs *kwerftv1.ConsoleSettings) map[string]any {
	out := map[string]any{"sso": nil, "secretSet": false, "redirectUrl": s.redirectURL(r),
		"available": s.cfg.SystemReader != nil && s.cfg.KubeCache != nil}
	if cs == nil {
		return out
	}
	out["secretSet"] = cs.Annotations[controllers.AnnotationOIDCSecretUpdated] != ""
	if c := cs.Spec.SSO; c != nil {
		v := ssoSettingsJSON{Enabled: c.Enabled, Provider: c.Provider, Issuer: c.Issuer, ClientID: c.ClientID,
			DisplayName: c.DisplayName, AllowedDomains: c.AllowedDomains, AutoJoin: c.AutoJoin, DefaultRole: c.DefaultRole}
		if v.AllowedDomains == nil {
			v.AllowedDomains = []string{}
		}
		if c.Provider == auth.OIDCMicrosoft {
			v.Tenant = strings.TrimSuffix(strings.TrimPrefix(c.Issuer, "https://login.microsoftonline.com/"), "/v2.0")
		}
		out["sso"] = v
	}
	return out
}

func (s *ssoAPI) settingsGet(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cs, err := (&settingsAPI{api: s.api}).load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.read", "settings", "Settings not found.", err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView(r, cs))
}

func (s *ssoAPI) settingsPut(w http.ResponseWriter, r *http.Request) {
	var req ssoSettingsJSON
	if !decode(w, r, &req) {
		return
	}
	spec := kwerftv1.SSOSettings{Enabled: req.Enabled, Provider: req.Provider, ClientID: strings.TrimSpace(req.ClientID),
		DisplayName: strings.TrimSpace(req.DisplayName), AutoJoin: req.AutoJoin, DefaultRole: req.DefaultRole}
	secret := strings.TrimSpace(req.ClientSecret)
	switch spec.Provider {
	case auth.OIDCGoogle:
		spec.Issuer = auth.GoogleIssuer
	case auth.OIDCMicrosoft:
		iss, err := auth.MicrosoftIssuer(req.Tenant)
		if err != nil {
			writeFieldError(w, "tenant", "Enter the directory (tenant) ID from Microsoft Entra, a GUID. Multi-tenant sign-in is not supported.")
			return
		}
		spec.Issuer = iss
	case auth.OIDCKeycloak, auth.OIDCGeneric:
		spec.Issuer = strings.TrimSuffix(strings.TrimSpace(req.Issuer), "/")
		if spec.Provider == auth.OIDCKeycloak && strings.HasSuffix(spec.Issuer, "/realms") {
			writeFieldError(w, "issuer", "Add the realm: https://<keycloak>/realms/<realm>.")
			return
		}
	default:
		writeFieldError(w, "provider", "Choose Google, Microsoft, Keycloak or another OpenID Connect provider.")
		return
	}
	if err := auth.CheckOIDCIssuer(spec.Provider, spec.Issuer); err != nil {
		writeFieldError(w, "issuer", "Enter the provider's issuer URL: "+err.Error()+".")
		return
	}
	if spec.ClientID == "" || len(spec.ClientID) > 500 || strings.ContainsAny(spec.ClientID, " \t\r\n") {
		writeFieldError(w, "clientId", "Enter the client ID of the OAuth client you created at the provider.")
		return
	}
	if len(secret) > 1000 || strings.ContainsAny(secret, "\r\n") {
		writeFieldError(w, "clientSecret", "That does not look like a client secret.")
		return
	}
	if utf8.RuneCountInString(spec.DisplayName) > 60 {
		writeFieldError(w, "displayName", "Use at most 60 characters.")
		return
	}
	for _, d := range req.AllowedDomains {
		d = normalizeHost(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d == "" || slices.Contains(spec.AllowedDomains, d) {
			continue
		}
		if !validHostname(d) {
			writeFieldError(w, "allowedDomains", d+" is not a domain. Enter domains like example.com.")
			return
		}
		spec.AllowedDomains = append(spec.AllowedDomains, d)
	}
	if len(spec.AllowedDomains) > 20 {
		writeFieldError(w, "allowedDomains", "Enter at most 20 domains.")
		return
	}
	if spec.AutoJoin && len(spec.AllowedDomains) == 0 {
		writeFieldError(w, "allowedDomains", "Auto-join needs at least one allowed domain, or anyone with an account at the provider could join.")
		return
	}
	switch spec.DefaultRole {
	case "":
		spec.DefaultRole = store.RoleViewer
	case store.RoleDeveloper, store.RoleViewer:
	default:
		writeFieldError(w, "defaultRole", "New members can join as developer or viewer.")
		return
	}

	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	settings := &settingsAPI{api: s.api}
	cs, err := settings.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.sso", "sso", "Settings not found.", err)
		return
	}
	secretSet := cs != nil && cs.Annotations[controllers.AnnotationOIDCSecretUpdated] != ""
	if spec.Enabled && secret == "" && !secretSet {
		writeFieldError(w, "clientSecret", "Enter the client secret of the OAuth client.")
		return
	}
	if spec.Enabled && secret == "" && cs != nil && cs.Spec.SSO != nil && (cs.Spec.SSO.Issuer != spec.Issuer || cs.Spec.SSO.ClientID != spec.ClientID) {
		// The stored secret belongs to the old client.
		writeFieldError(w, "clientSecret", "Enter the client secret of the new OAuth client.")
		return
	}
	if spec.Enabled {
		// The provider must answer before sign-in depends on it.
		s.oidc.Forget()
		if _, err := s.oidc.Discover(ctx, spec.Issuer); err != nil {
			writeFieldError(w, "issuer", "Could not read "+spec.Issuer+"/.well-known/openid-configuration: "+truncate(err.Error(), 200))
			return
		}
	}
	if secret != "" {
		if cs == nil {
			// The Domain reconciler creates the Secret once settings exist.
			if err := settings.patchSettings(ctx, c, nil, map[string]any{}); err != nil {
				s.kubeError(w, r, p, "settings.sso", "sso", "Settings not found.", err)
				return
			}
			if cs, err = settings.load(ctx, c); err != nil {
				s.internalError(w, r, err)
				return
			}
		}
		if err := writeSecretKey(ctx, c, controllers.OIDCSecret, controllers.OIDCSecretKey, secret); err != nil {
			if apierrors.IsNotFound(err) {
				writeError(w, http.StatusServiceUnavailable, "The console is still preparing the secret's storage. Try again in a moment.")
				return
			}
			s.kubeError(w, r, p, "settings.sso_secret", controllers.OIDCSecret, "Secret storage not found.", err)
			return
		}
		s.audit(r, p.user.Email, "settings.sso_secret", spec.Provider, "client secret replaced")
	}
	patch := map[string]any{"spec": map[string]any{"sso": spec}}
	if secret != "" {
		patch["metadata"] = map[string]any{"annotations": map[string]any{
			controllers.AnnotationOIDCSecretUpdated: s.now().UTC().Format(time.RFC3339Nano)}}
	}
	if err := settings.patchSettings(ctx, c, cs, patch); err != nil {
		s.kubeError(w, r, p, "settings.sso", "sso", "Settings not found.", err)
		return
	}
	detail := "off"
	if spec.Enabled {
		detail = "on"
	}
	detail += ", " + spec.Provider + " " + spec.Issuer
	if spec.AutoJoin {
		detail += ", auto-join as " + spec.DefaultRole + " for " + strings.Join(spec.AllowedDomains, ", ")
	}
	s.audit(r, p.user.Email, "settings.sso", spec.Provider, detail)
	if cs, err = settings.load(ctx, c); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView(r, cs))
}

// writeSecretKey patches one key of a write-only Secret in kwerft-system as
// the user, waiting briefly for the reconciler to create it.
func writeSecretKey(ctx context.Context, c client.Client, name, key, value string) error {
	raw, err := json.Marshal(map[string]any{"data": map[string]string{key: base64.StdEncoding.EncodeToString([]byte(value))}})
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: name}}
	for deadline := time.Now().Add(5 * time.Second); ; {
		err = c.Patch(ctx, sec, client.RawPatch(types.MergePatchType, raw))
		if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---- Account › single sign-on ----------------------------------------------------

func (s *ssoAPI) unlink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Issuer   string `json:"issuer"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := principalOf(r)
	if p.user.PasswordHash == "" {
		writeError(w, http.StatusConflict, "Set a password first: single sign-on is the only way into this account.")
		return
	}
	if !s.confirmIdentity(w, r, p.user, req.Password) {
		return
	}
	if err := s.store.UnlinkIdentity(r.Context(), p.user.ID, req.Issuer); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "That sign-in is not linked. Reload the page.")
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, p.user.Email, "account.sso_unlinked", p.user.Email, req.Issuer)
	w.WriteHeader(http.StatusNoContent)
}

// identitiesJSON lists the user's provider links for the account page.
func (a *api) identitiesJSON(ctx context.Context, userID string) ([]map[string]any, error) {
	list, err := a.store.Identities(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(list))
	for _, i := range list {
		out = append(out, map[string]any{"issuer": i.Issuer, "email": i.Email,
			"linkedAt": i.CreatedAt.UTC().Format(time.RFC3339), "lastLoginAt": i.LastLoginAt.UTC().Format(time.RFC3339)})
	}
	return out, nil
}

// freshSession reports whether the request's session signed in recently
// enough to stand in for a password.
func (a *api) freshSession(r *http.Request) bool {
	p := principalOf(r)
	return p.token == nil && !p.sessionCreated.IsZero() && a.now().Sub(p.sessionCreated) <= freshSignIn
}
