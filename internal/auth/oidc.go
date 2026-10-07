// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Single sign-on through OpenID Connect: the authorization code flow with
// PKCE (S256), a state bound to the browser and a nonce bound to the ID
// token. coreos/go-oidc does discovery and verifies ID tokens (signature
// against the provider's published keys, issuer, audience, expiry);
// x/oauth2 does the code exchange.
//
// Users are matched by email, so the email must be one the provider vouches
// for. Generic OIDC providers say so in email_verified, which must be true.
// Microsoft Entra ID has no email_verified; its preset therefore requires a
// single tenant (whose administrators control the addresses) and takes
// email, or else preferred_username when it is an address. GitHub is OAuth2
// without OpenID Connect and is not supported directly; put an OIDC broker
// (Keycloak, Dex) in front of it.

// Provider presets.
const (
	OIDCGoogle    = "google"
	OIDCMicrosoft = "microsoft"
	OIDCKeycloak  = "keycloak"
	OIDCGeneric   = "oidc"
)

// GoogleIssuer is Google's (fixed) issuer.
const GoogleIssuer = "https://accounts.google.com"

var (
	ErrOIDCUnverified = errors.New("the provider did not confirm the email address")
	ErrOIDCNoEmail    = errors.New("the provider sent no email address")

	entraIssuerRE = regexp.MustCompile(`^https://login\.microsoftonline\.com/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/v2\.0$`)
	entraTenantRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// MicrosoftIssuer is the issuer of a Microsoft Entra tenant (its directory ID).
func MicrosoftIssuer(tenant string) (string, error) {
	tenant = strings.TrimSpace(tenant)
	if !entraTenantRE.MatchString(tenant) {
		return "", errors.New("enter the directory (tenant) ID, a GUID like 00000000-0000-0000-0000-000000000000; " +
			"multi-tenant endpoints (common, organizations) are not allowed")
	}
	return "https://login.microsoftonline.com/" + strings.ToLower(tenant) + "/v2.0", nil
}

// CheckOIDCIssuer validates an issuer URL for a preset.
func CheckOIDCIssuer(provider, issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || len(issuer) > 500 {
		return errors.New("the issuer must be an https:// URL without query or fragment")
	}
	switch provider {
	case OIDCGoogle:
		if issuer != GoogleIssuer {
			return fmt.Errorf("Google's issuer is %s", GoogleIssuer)
		}
	case OIDCMicrosoft:
		if !entraIssuerRE.MatchString(issuer) {
			return errors.New("a Microsoft Entra issuer names one tenant: https://login.microsoftonline.com/<tenant ID>/v2.0")
		}
	case OIDCKeycloak, OIDCGeneric:
	default:
		return fmt.Errorf("unknown provider %q", provider)
	}
	return nil
}

// OIDCConfig is one provider as the console uses it.
type OIDCConfig struct {
	Provider     string // a preset
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// OIDCFlow is one sign-in in progress: what the browser must bring back.
type OIDCFlow struct {
	State    string
	Nonce    string
	Verifier string // PKCE code verifier
}

// NewOIDCFlow returns fresh random values for a sign-in.
func NewOIDCFlow() OIDCFlow {
	return OIDCFlow{State: NewToken(), Nonce: NewToken(), Verifier: oauth2.GenerateVerifier()}
}

// OIDCIdentity is what a provider asserted about the user.
type OIDCIdentity struct {
	Issuer  string
	Subject string
	Email   string // lower-cased; verified (see the package comment)
	Name    string
}

// OIDC caches discovery documents per issuer. HTTP is the client for the
// provider (discovery, keys, code exchange); nil means a client with a 15 s
// timeout.
type OIDC struct {
	HTTP *http.Client
	Now  func() time.Time

	mu        sync.Mutex
	providers map[string]cachedProvider
}

type cachedProvider struct {
	p       *oidc.Provider
	fetched time.Time
}

// discoveryTTL is how long a discovery document is reused. Keys rotate
// independently: go-oidc refetches them when a token names an unknown one.
const discoveryTTL = time.Hour

func (o *OIDC) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OIDC) ctx(ctx context.Context) context.Context {
	c := o.HTTP
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	return oidc.ClientContext(ctx, c)
}

// Discover returns the provider for issuer, from cache or its discovery
// document. The document's issuer must equal issuer exactly.
func (o *OIDC) Discover(ctx context.Context, issuer string) (*oidc.Provider, error) {
	o.mu.Lock()
	if c, ok := o.providers[issuer]; ok && o.now().Sub(c.fetched) < discoveryTTL {
		o.mu.Unlock()
		return c.p, nil
	}
	o.mu.Unlock()
	p, err := oidc.NewProvider(o.ctx(ctx), issuer)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.providers == nil {
		o.providers = map[string]cachedProvider{}
	}
	o.providers[issuer] = cachedProvider{p: p, fetched: o.now()}
	return p, nil
}

// Forget drops a cached discovery document (after the settings change).
func (o *OIDC) Forget() {
	o.mu.Lock()
	o.providers = nil
	o.mu.Unlock()
}

func oauthConfig(p *oidc.Provider, cfg OIDCConfig) *oauth2.Config {
	return &oauth2.Config{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
		Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile"},
	}
}

// AuthURL is where the browser goes to sign in at the provider.
func (o *OIDC) AuthURL(ctx context.Context, cfg OIDCConfig, flow OIDCFlow) (string, error) {
	p, err := o.Discover(ctx, cfg.Issuer)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier)}
	if cfg.Provider == OIDCGoogle || cfg.Provider == OIDCMicrosoft {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "select_account"))
	}
	return oauthConfig(p, cfg).AuthCodeURL(flow.State, opts...), nil
}

// Finish exchanges the code the provider sent back, verifies the ID token
// (signature, issuer, audience, expiry, nonce) and returns who signed in. The
// caller has already matched the state.
func (o *OIDC) Finish(ctx context.Context, cfg OIDCConfig, flow OIDCFlow, code string) (*OIDCIdentity, error) {
	p, err := o.Discover(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	hctx := o.ctx(ctx)
	tok, err := oauthConfig(p, cfg).Exchange(hctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, errors.New("the provider returned no ID token")
	}
	idt, err := p.VerifierContext(hctx, &oidc.Config{ClientID: cfg.ClientID, Now: o.now}).Verify(hctx, raw)
	if err != nil {
		return nil, fmt.Errorf("ID token: %w", err)
	}
	if idt.Nonce == "" || !TokenMatches(idt.Nonce, HashToken(flow.Nonce)) {
		return nil, errors.New("ID token: nonce does not match this sign-in")
	}
	var claims struct {
		Email             string `json:"email"`
		EmailVerified     any    `json:"email_verified"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idt.Claims(&claims); err != nil {
		return nil, fmt.Errorf("ID token claims: %w", err)
	}
	id := &OIDCIdentity{Issuer: idt.Issuer, Subject: idt.Subject, Name: strings.TrimSpace(claims.Name)}
	email, verified := strings.TrimSpace(claims.Email), isTrue(claims.EmailVerified)
	if cfg.Provider == OIDCMicrosoft {
		// One tenant (CheckOIDCIssuer): its administrators assign addresses.
		if email == "" && strings.Contains(claims.PreferredUsername, "@") {
			email = strings.TrimSpace(claims.PreferredUsername)
		}
		verified = true
	}
	if id.Subject == "" {
		return nil, errors.New("ID token without a subject")
	}
	if email == "" {
		return id, ErrOIDCNoEmail
	}
	id.Email = strings.ToLower(email)
	if !verified {
		return id, ErrOIDCUnverified
	}
	return id, nil
}

// isTrue reads email_verified, which some providers send as a string.
func isTrue(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}
