package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/auth/oidctest"
	"github.com/ehilzinger/kwerft/internal/store"
)

// ssoEnv is a console whose single sign-on points at a fake provider.
type ssoEnv struct {
	*env
	iss *oidctest.Issuer
	cfg *ssoConfig
}

func newSSOEnv(t *testing.T, opts ...func(*Config)) *ssoEnv {
	t.Helper()
	iss := oidctest.New(t)
	s := &ssoEnv{iss: iss, cfg: &ssoConfig{SSOSettings: kwerftv1.SSOSettings{Enabled: true, Provider: auth.OIDCKeycloak,
		Issuer: iss.URL, ClientID: iss.ClientID, DisplayName: "Test SSO"}, ClientSecret: iss.ClientSecret}}
	opts = append(opts, func(c *Config) {
		c.ssoHook = func(a *ssoAPI) {
			a.oidc.HTTP = iss.Client()
			a.load = func(context.Context) (*ssoConfig, error) {
				if s.cfg == nil {
					return nil, nil
				}
				cp := *s.cfg
				return &cp, nil
			}
		}
	})
	s.env = newEnv(t, opts...)
	iss.Now = s.env.clock.Now
	return s
}

// browser is a client that does not follow redirects, like a test of each
// hop.
func (s *ssoEnv) browser() *env {
	jar, _ := cookiejar.New(nil)
	c := *s.env
	c.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &c
}

func browse(t *testing.T, b *env, u string) *http.Response {
	t.Helper()
	if strings.HasPrefix(u, "/") {
		u = b.srv.URL + u
	}
	res, err := b.client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

// signIn runs the whole flow in browser b and returns where the console
// sent the browser in the end.
func (s *ssoEnv) signIn(t *testing.T, b *env, claims map[string]any) string {
	t.Helper()
	res := browse(t, b, "/api/v1/sso/start")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, s.iss.URL+"/authorize?") {
		return loc // refused before the provider
	}
	back := s.iss.Authorize(t, loc, oidctest.Login{Claims: claims})
	if !strings.HasPrefix(back, b.srv.URL+"/api/v1/sso/callback?") {
		t.Fatalf("redirect URI %s", back)
	}
	res = browse(t, b, back)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d", res.StatusCode)
	}
	return res.Header.Get("Location")
}

func verified(email, sub string) map[string]any {
	return map[string]any{"email": email, "email_verified": true, "sub": sub, "name": "Someone"}
}

func TestSSOStatusNeverShowsTheSecret(t *testing.T) {
	s := newSSOEnv(t)
	code, out := s.call(t, "GET", "/api/v1/sso", nil, nil)
	if code != http.StatusOK || out["enabled"] != true || out["displayName"] != "Test SSO" || strings.Contains(stringify(out), s.iss.ClientSecret) {
		t.Errorf("status %d %v", code, out)
	}
	s.cfg.Enabled = false
	if _, out := s.call(t, "GET", "/api/v1/sso", nil, nil); out["enabled"] != false {
		t.Errorf("disabled: %v", out)
	}
	s.cfg.Enabled, s.cfg.ClientSecret = true, ""
	if _, out := s.call(t, "GET", "/api/v1/sso", nil, nil); out["enabled"] != false {
		t.Errorf("without a secret: %v", out)
	}
	if loc := s.signIn(t, s.browser(), nil); loc != "/login?sso=unavailable" {
		t.Errorf("start while off: %s", loc)
	}
}

func TestSSOSignsInAnExistingMemberAndLinksThem(t *testing.T) {
	s := newSSOEnv(t)
	s.completeSetup(t)
	b := s.browser()
	if loc := s.signIn(t, b, verified("MARA@example.com", "sub-mara")); loc != "/" {
		t.Fatalf("sign-in ended at %s", loc)
	}
	if code, out := b.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusOK || out["email"] != owner["email"] {
		t.Fatalf("session: %d %v", code, out)
	}
	if a := lastAudit(t, s.store, "session.login"); a == nil || a.Detail != "single sign-on (Test SSO)" {
		t.Errorf("login audit %+v", a)
	}
	if a := lastAudit(t, s.store, "account.sso_linked"); a == nil || a.Detail != s.iss.URL {
		t.Errorf("link audit %+v", a)
	}
	_, acc := b.call(t, "GET", "/api/v1/account", nil, nil)
	if ids, _ := acc["identities"].([]any); len(ids) != 1 || acc["hasPassword"] != true {
		t.Errorf("account %v", acc)
	}
	// The link holds even after the provider's email changes.
	if loc := s.signIn(t, s.browser(), verified("mara.lindqvist@example.com", "sub-mara")); loc != "/" {
		t.Errorf("second sign-in: %s", loc)
	}
	// Another provider account claiming the same email is not linked.
	if loc := s.signIn(t, s.browser(), verified(owner["email"], "sub-impostor")); loc != "/login?sso=conflict" {
		t.Errorf("second subject: %s", loc)
	}
}

func TestSSORefusals(t *testing.T) {
	s := newSSOEnv(t)
	s.completeSetup(t)
	for name, c := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"unverified email": {map[string]any{"email": owner["email"], "email_verified": false}, "/login?sso=unverified"},
		"no email":         {map[string]any{"email_verified": true}, "/login?sso=unverified"},
		"not a member":     {verified("stranger@example.com", "s1"), "/login?sso=not_member"},
		"wrong audience":   {map[string]any{"email": owner["email"], "email_verified": true, "aud": "other-app"}, "/login?sso=failed"},
	} {
		if loc := s.signIn(t, s.browser(), c.claims); loc != c.want {
			t.Errorf("%s: %s, want %s", name, loc, c.want)
		}
	}
	if a := lastAudit(t, s.store, "session.sso_failed"); a == nil {
		t.Error("refusals not audited")
	}
	// Allowed domains apply to every sign-in, members included.
	s.cfg.AllowedDomains = []string{"corp.example"}
	if loc := s.signIn(t, s.browser(), verified(owner["email"], "sub-mara")); loc != "/login?sso=domain" {
		t.Errorf("member outside the allowed domains: %s", loc)
	}
}

func TestSSOFlowIsBoundToTheBrowser(t *testing.T) {
	s := newSSOEnv(t)
	s.completeSetup(t)
	start := func(b *env) string {
		res := browse(t, b, "/api/v1/sso/start")
		return s.iss.Authorize(t, res.Header.Get("Location"), oidctest.Login{Claims: verified(owner["email"], "sub-mara")})
	}
	// The callback in another browser (no flow cookie): refused.
	back := start(s.browser())
	if res := browse(t, s.browser(), back); res.Header.Get("Location") != "/login?sso=expired" {
		t.Errorf("other browser: %s", res.Header.Get("Location"))
	}
	// A forged state: refused, and the flow is used up.
	b := s.browser()
	back = start(b)
	u, _ := url.Parse(back)
	q := u.Query()
	q.Set("state", "forged")
	u.RawQuery = q.Encode()
	if res := browse(t, b, u.String()); res.Header.Get("Location") != "/login?sso=expired" {
		t.Errorf("forged state: %s", res.Header.Get("Location"))
	}
	if res := browse(t, b, back); res.Header.Get("Location") != "/login?sso=expired" {
		t.Errorf("flow reused after a failure: %s", res.Header.Get("Location"))
	}
	// Replaying a successful callback does not sign in twice.
	b = s.browser()
	back = start(b)
	if res := browse(t, b, back); res.Header.Get("Location") != "/" {
		t.Fatalf("callback: %s", res.Header.Get("Location"))
	}
	if res := browse(t, b, back); res.Header.Get("Location") != "/login?sso=expired" {
		t.Errorf("replay: %s", res.Header.Get("Location"))
	}
	// The flow expires.
	b = s.browser()
	back = start(b)
	s.clock.Advance(11 * time.Minute)
	if res := browse(t, b, back); res.Header.Get("Location") != "/login?sso=expired" {
		t.Errorf("late callback: %s", res.Header.Get("Location"))
	}
	// The provider refused (or the user cancelled).
	b = s.browser()
	res := browse(t, b, "/api/v1/sso/start")
	authURL, _ := url.Parse(res.Header.Get("Location"))
	cb := b.srv.URL + "/api/v1/sso/callback?error=access_denied&state=" + url.QueryEscape(authURL.Query().Get("state"))
	if res := browse(t, b, cb); res.Header.Get("Location") != "/login?sso=denied" {
		t.Errorf("provider error: %s", res.Header.Get("Location"))
	}
}

func TestSSOAcceptsAnInviteAndMakesAPasswordlessAccount(t *testing.T) {
	s := newSSOEnv(t, func(c *Config) { c.DataKey = testDataKey })
	s.completeSetup(t)
	s.invite(t, "sam@example.com", store.RoleDeveloper)
	b := s.browser()
	if loc := s.signIn(t, b, verified("sam@example.com", "sub-sam")); loc != "/" {
		t.Fatalf("invited sign-in: %s", loc)
	}
	sam := mustUser(t, s.store, "sam@example.com")
	if sam.Role != store.RoleDeveloper || sam.PasswordHash != "" || sam.Name != "Someone" {
		t.Errorf("user %+v", sam)
	}
	if a := lastAudit(t, s.store, "member.invite_accepted"); a == nil || !strings.Contains(a.Detail, "single sign-on") {
		t.Errorf("audit %+v", a)
	}
	// No password: password sign-in fails like a wrong one.
	if code, _ := s.newClient().call(t, "POST", "/api/v1/session", map[string]string{"email": "sam@example.com", "password": ""}, nil); code != http.StatusUnauthorized {
		t.Errorf("empty password: %d", code)
	}
	// A fresh sign-in stands in for the password: set one, or a factor.
	if code, out := b.call(t, "POST", "/api/v1/account/totp", map[string]string{"password": ""}, nil); code != http.StatusOK {
		t.Errorf("TOTP setup right after signing in: %d %v", code, out)
	}
	s.clock.Advance(11 * time.Minute)
	if code, out := b.call(t, "POST", "/api/v1/account/recovery-codes", map[string]string{"password": ""}, nil); code != http.StatusBadRequest || out["field"] != "password" {
		t.Errorf("confirmation 11 minutes later: %d %v", code, out)
	}
	if code, out := b.call(t, "POST", "/api/v1/account/password", map[string]string{"new": "a brand new password"}, nil); code != http.StatusBadRequest || out["field"] != "current" {
		t.Errorf("first password, stale sign-in: %d %v", code, out)
	}
	b = s.browser()
	if loc := s.signIn(t, b, verified("sam@example.com", "sub-sam")); loc != "/" {
		t.Fatal(loc)
	}
	if code, out := b.call(t, "POST", "/api/v1/account/password", map[string]string{"new": "a brand new password"}, nil); code != http.StatusOK {
		t.Errorf("first password: %d %v", code, out)
	}
	if code, _ := s.newClient().call(t, "POST", "/api/v1/session", map[string]string{"email": "sam@example.com", "password": "a brand new password"}, nil); code != http.StatusOK {
		t.Errorf("password sign-in after setting one: %d", code)
	}
	// With a password, the link can be removed.
	if code, _ := b.call(t, "POST", "/api/v1/account/sso/unlink", map[string]string{"issuer": s.iss.URL, "password": "a brand new password"}, nil); code != http.StatusNoContent {
		t.Errorf("unlink: %d", code)
	}
}

func TestSSOAutoJoin(t *testing.T) {
	s := newSSOEnv(t)
	s.completeSetup(t)
	// Without allowed domains auto-join never applies.
	s.cfg.AutoJoin = true
	if loc := s.signIn(t, s.browser(), verified("new@corp.example", "n1")); loc != "/login?sso=not_member" {
		t.Errorf("auto-join without domains: %s", loc)
	}
	s.cfg.AllowedDomains, s.cfg.DefaultRole = []string{"corp.example"}, "admin" // anything above developer becomes viewer
	if loc := s.signIn(t, s.browser(), verified("new@corp.example", "n1")); loc != "/" {
		t.Fatalf("auto-join: %s", loc)
	}
	if u := mustUser(t, s.store, "new@corp.example"); u.Role != store.RoleViewer {
		t.Errorf("auto-joined as %s", u.Role)
	}
	if a := lastAudit(t, s.store, "member.auto_joined"); a == nil {
		t.Error("auto-join not audited")
	}
	s.cfg.DefaultRole = store.RoleDeveloper
	if loc := s.signIn(t, s.browser(), verified("dev@corp.example", "n2")); loc != "/" {
		t.Fatal(loc)
	}
	if u := mustUser(t, s.store, "dev@corp.example"); u.Role != store.RoleDeveloper {
		t.Errorf("auto-joined as %s", u.Role)
	}
	if loc := s.signIn(t, s.browser(), verified("x@other.example", "n3")); loc != "/login?sso=domain" {
		t.Errorf("other domain: %s", loc)
	}
	if _, err := s.store.UserByEmail(context.Background(), "x@other.example"); !errors.Is(err, store.ErrNotFound) {
		t.Error("account created outside the allowed domains")
	}
}

func TestSSOReturnsToTheConsolePageOnly(t *testing.T) {
	s := newSSOEnv(t)
	s.completeSetup(t)
	for next, want := range map[string]string{
		"/apps/shop/web?tab=logs": "/apps/shop/web?tab=logs",
		"//evil.example/x":        "/",
		"/\\evil.example":         "/",
		"https://evil.example":    "/",
		"":                        "/",
	} {
		b := s.browser()
		res := browse(t, b, "/api/v1/sso/start?next="+url.QueryEscape(next))
		back := s.iss.Authorize(t, res.Header.Get("Location"), oidctest.Login{Claims: verified(owner["email"], "sub-mara")})
		if res := browse(t, b, back); res.Header.Get("Location") != want {
			t.Errorf("next %q: ended at %s", next, res.Header.Get("Location"))
		}
	}
}

// A member's own second factors still apply after the provider.
func TestSSOStillAsksForTheMembersSecondFactor(t *testing.T) {
	s := newSSOEnv(t, func(c *Config) { c.DataKey = testDataKey })
	s.completeSetup(t)
	secret, _ := s.enrollTOTP(t)
	s.clock.Advance(30 * time.Second) // a code works once
	b := s.browser()
	loc := s.signIn(t, b, verified(owner["email"], "sub-mara"))
	if loc != "/login?second-factor=totp%2Crecovery" {
		t.Fatalf("after the provider: %s", loc)
	}
	if code, _ := b.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("signed in before the second factor: %d", code)
	}
	code, out := b.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": auth.TOTPCode(secret, s.clock.Now())}, nil)
	if code != http.StatusOK {
		t.Fatalf("second factor: %d %v", code, out)
	}
	if a := lastAudit(t, s.store, "session.login"); a == nil || a.Detail != "single sign-on (Test SSO) + authenticator app" {
		t.Errorf("audit %+v", a)
	}
}
