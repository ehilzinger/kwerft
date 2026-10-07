// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package auth_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/auth/oidctest"
)

func oidcSetup(t *testing.T) (*oidctest.Issuer, *auth.OIDC, auth.OIDCConfig) {
	iss := oidctest.New(t)
	o := &auth.OIDC{HTTP: iss.Client()}
	cfg := auth.OIDCConfig{Provider: auth.OIDCKeycloak, Issuer: iss.URL, ClientID: iss.ClientID, ClientSecret: iss.ClientSecret,
		RedirectURL: "https://console.example.com/api/v1/sso/callback"}
	return iss, o, cfg
}

// signIn runs the flow: auth URL, the provider's redirect back, Finish.
func signIn(t *testing.T, iss *oidctest.Issuer, o *auth.OIDC, cfg auth.OIDCConfig, login oidctest.Login, mutate func(*auth.OIDCFlow, *string)) (*auth.OIDCIdentity, error) {
	t.Helper()
	ctx := context.Background()
	flow := auth.NewOIDCFlow()
	authURL, err := o.AuthURL(ctx, cfg, flow)
	if err != nil {
		t.Fatal(err)
	}
	back, _ := url.Parse(iss.Authorize(t, authURL, login))
	if back.Query().Get("state") != flow.State {
		t.Fatalf("state not echoed")
	}
	code := back.Query().Get("code")
	if mutate != nil {
		mutate(&flow, &code)
	}
	return o.Finish(ctx, cfg, flow, code)
}

func TestOIDCCodeFlow(t *testing.T) {
	iss, o, cfg := oidcSetup(t)
	id, err := signIn(t, iss, o, cfg, oidctest.Login{Claims: map[string]any{
		"email": "Mara@Example.com", "email_verified": true, "name": "Mara Lindqvist"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id.Issuer != iss.URL || id.Subject != "subject-1" || id.Email != "mara@example.com" || id.Name != "Mara Lindqvist" {
		t.Errorf("identity %+v", id)
	}
	// email_verified as a string, as some providers send it.
	if _, err := signIn(t, iss, o, cfg, oidctest.Login{Claims: map[string]any{"email": "a@example.com", "email_verified": "true"}}, nil); err != nil {
		t.Errorf("string email_verified: %v", err)
	}
}

func TestOIDCRefusesBadTokensAndFlows(t *testing.T) {
	iss, o, cfg := oidcSetup(t)
	ok := map[string]any{"email": "mara@example.com", "email_verified": true}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range ok {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, tc := range map[string]struct {
		login  oidctest.Login
		mutate func(*auth.OIDCFlow, *string)
		want   error
	}{
		"unverified email":    {login: oidctest.Login{Claims: with(map[string]any{"email_verified": false})}, want: auth.ErrOIDCUnverified},
		"no email_verified":   {login: oidctest.Login{Claims: with(map[string]any{"email_verified": nil})}, want: auth.ErrOIDCUnverified},
		"no email":            {login: oidctest.Login{Claims: map[string]any{"email_verified": true}}, want: auth.ErrOIDCNoEmail},
		"foreign signing key": {login: oidctest.Login{Claims: ok, ForeignKey: true}},
		"other audience":      {login: oidctest.Login{Claims: with(map[string]any{"aud": "someone-else"})}},
		"other issuer":        {login: oidctest.Login{Claims: with(map[string]any{"iss": "https://evil.example.com"})}},
		"expired":             {login: oidctest.Login{Claims: with(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})}},
		"replayed nonce":      {login: oidctest.Login{Claims: with(map[string]any{"nonce": "another-sign-in"})}},
		"missing nonce":       {login: oidctest.Login{Claims: with(map[string]any{"nonce": nil})}},
		"wrong PKCE verifier": {login: oidctest.Login{Claims: ok}, mutate: func(f *auth.OIDCFlow, _ *string) { f.Verifier = auth.NewOIDCFlow().Verifier }},
		"other flow's nonce":  {login: oidctest.Login{Claims: ok}, mutate: func(f *auth.OIDCFlow, _ *string) { f.Nonce = "x" }},
		"forged code":         {login: oidctest.Login{Claims: ok}, mutate: func(_ *auth.OIDCFlow, c *string) { *c = "forged" }},
	} {
		t.Run(name, func(t *testing.T) {
			id, err := signIn(t, iss, o, cfg, tc.login, tc.mutate)
			if err == nil {
				t.Fatalf("accepted: %+v", id)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err %v, want %v", err, tc.want)
			}
		})
	}
	// A code works once.
	ctx := context.Background()
	flow := auth.NewOIDCFlow()
	authURL, _ := o.AuthURL(ctx, cfg, flow)
	back, _ := url.Parse(iss.Authorize(t, authURL, oidctest.Login{Claims: ok}))
	if _, err := o.Finish(ctx, cfg, flow, back.Query().Get("code")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Finish(ctx, cfg, flow, back.Query().Get("code")); err == nil {
		t.Error("code accepted twice")
	}
	// A wrong client secret fails the exchange.
	bad := cfg
	bad.ClientSecret = "wrong"
	if _, err := signIn(t, iss, o, bad, oidctest.Login{Claims: ok}, nil); err == nil || !strings.Contains(err.Error(), "exchange") {
		t.Errorf("wrong client secret: %v", err)
	}
}

// Microsoft Entra sends no email_verified: one tenant is trusted, and
// preferred_username stands in for a missing email.
func TestOIDCMicrosoftPreset(t *testing.T) {
	iss, o, cfg := oidcSetup(t)
	cfg.Provider = auth.OIDCMicrosoft
	id, err := signIn(t, iss, o, cfg, oidctest.Login{Claims: map[string]any{"preferred_username": "Sam@Contoso.com"}}, nil)
	if err != nil || id.Email != "sam@contoso.com" {
		t.Errorf("entra: %+v %v", id, err)
	}
	if _, err := auth.MicrosoftIssuer("common"); err == nil {
		t.Error("multi-tenant endpoint accepted")
	}
	issuer, err := auth.MicrosoftIssuer("8F7C2C1E-1111-2222-3333-444455556666")
	if err != nil || issuer != "https://login.microsoftonline.com/8f7c2c1e-1111-2222-3333-444455556666/v2.0" {
		t.Errorf("issuer %q %v", issuer, err)
	}
	if err := auth.CheckOIDCIssuer(auth.OIDCMicrosoft, issuer); err != nil {
		t.Error(err)
	}
}

func TestCheckOIDCIssuer(t *testing.T) {
	for _, tc := range []struct {
		provider, issuer string
		ok               bool
	}{
		{auth.OIDCGoogle, "https://accounts.google.com", true},
		{auth.OIDCGoogle, "https://accounts.google.com.evil.example", false},
		{auth.OIDCKeycloak, "https://sso.example.com/realms/ops", true},
		{auth.OIDCGeneric, "http://sso.example.com", false},
		{auth.OIDCGeneric, "https://sso.example.com/?x=1", false},
		{auth.OIDCGeneric, "https://user@sso.example.com", false},
		{auth.OIDCMicrosoft, "https://login.microsoftonline.com/common/v2.0", false},
		{"github", "https://github.com", false},
	} {
		if err := auth.CheckOIDCIssuer(tc.provider, tc.issuer); (err == nil) != tc.ok {
			t.Errorf("%s %s: %v", tc.provider, tc.issuer, err)
		}
	}
}
