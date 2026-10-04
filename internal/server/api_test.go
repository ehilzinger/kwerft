package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/setup"
	"github.com/ehilzinger/kwerft/internal/store"
)

const testToken = "kwft_setup_abcdefghijklmnopqrstuvwx"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	srv    *httptest.Server
	client *http.Client
	store  *store.Store
	tokens *setup.StaticTokenSource
	clock  *fakeClock
}

// newEnv starts a console on a fresh database; opts adjust its Config.
func newEnv(t *testing.T, opts ...func(*Config)) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := &fakeClock{t: time.Now()}
	tokens := setup.NewStaticTokenSource(testToken, 24*time.Hour)
	cfg := Config{
		UI: fstest.MapFS{"index.html": {Data: []byte("ui")}}, Logger: slog.New(slog.DiscardHandler),
		Store: st, SetupTokens: tokens, InsecureCookies: true, Now: clock.Now,
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv := httptest.NewServer(Handler(cfg))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{srv: srv, client: &http.Client{Jar: jar}, store: st, tokens: tokens, clock: clock}
}

// call sends a JSON request and decodes a JSON response into out (if non-nil).
func (e *env) call(t *testing.T, method, path string, body any, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

var owner = map[string]string{"name": "Mara Lindqvist", "email": "mara@example.com", "password": "correct horse battery"}

// completeSetup runs the wizard's two calls and returns the owner response.
func (e *env) completeSetup(t *testing.T) map[string]any {
	t.Helper()
	if code, out := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, nil); code != http.StatusNoContent {
		t.Fatalf("verify: %d %v", code, out)
	}
	code, out := e.call(t, "POST", "/api/v1/setup/owner", owner, nil)
	if code != http.StatusCreated {
		t.Fatalf("owner: %d %v", code, out)
	}
	return out
}

func TestSetupFlowCreatesOwnerAndSignsIn(t *testing.T) {
	e := newEnv(t)
	if _, out := e.call(t, "GET", "/api/v1/setup", nil, nil); out["complete"] != false {
		t.Fatalf("fresh install reports complete: %v", out)
	}
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("session before setup: %d, want 401", code)
	}

	got := e.completeSetup(t)
	if got["role"] != store.RoleOwner || got["email"] != owner["email"] {
		t.Errorf("owner = %v", got)
	}
	if _, out := e.call(t, "GET", "/api/v1/setup", nil, nil); out["complete"] != true {
		t.Errorf("setup not complete after owner creation: %v", out)
	}
	// Signed in straight away.
	if code, out := e.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusOK || out["name"] != owner["name"] {
		t.Errorf("session after setup: %d %v", code, out)
	}
	// The token is single use.
	if _, _, err := e.tokens.Hash(context.Background()); err != setup.ErrNoToken {
		t.Errorf("token not consumed: %v", err)
	}
	// And the audit log has the story.
	code, _ := e.call(t, "GET", "/api/v1/audit", nil, nil)
	entries, _ := e.store.RecentAudit(context.Background(), 10)
	if code != http.StatusOK || len(entries) < 2 || entries[0].Action != "setup.owner_created" || entries[1].Action != "setup.token_accepted" {
		t.Errorf("audit: %d %+v", code, entries)
	}
}

func TestSetupRejectsWrongExpiredOrMissingToken(t *testing.T) {
	e := newEnv(t)
	if code, out := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": "kwft_setup_wrong"}, nil); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d %v", code, out)
	}
	if code, _ := e.call(t, "POST", "/api/v1/setup/owner", owner, nil); code != http.StatusUnauthorized {
		t.Errorf("owner without a verified token: %d, want 401", code)
	}
	e.clock.Advance(25 * time.Hour)
	if code, out := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, nil); code != http.StatusGone {
		t.Errorf("expired token: %d %v", code, out)
	}
}

func TestSetupGrantExpires(t *testing.T) {
	e := newEnv(t)
	e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, nil)
	e.clock.Advance(31 * time.Minute)
	if code, _ := e.call(t, "POST", "/api/v1/setup/owner", owner, nil); code != http.StatusUnauthorized {
		t.Errorf("owner after grant expiry: %d, want 401", code)
	}
}

func TestSetupValidatesOwnerFields(t *testing.T) {
	e := newEnv(t)
	e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, nil)
	cases := []struct {
		field string
		body  map[string]string
	}{
		{"name", map[string]string{"name": " ", "email": "a@example.com", "password": "correct horse battery"}},
		{"email", map[string]string{"name": "A", "email": "not-an-email", "password": "correct horse battery"}},
		{"password", map[string]string{"name": "A", "email": "a@example.com", "password": "short"}},
	}
	for _, c := range cases {
		code, out := e.call(t, "POST", "/api/v1/setup/owner", c.body, nil)
		if code != http.StatusBadRequest || out["field"] != c.field {
			t.Errorf("%s: %d %v", c.field, code, out)
		}
	}
}

func TestSecondSetupIsRefused(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	if code, _ := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, nil); code != http.StatusConflict {
		t.Errorf("verify after setup: %d, want 409", code)
	}
}

func TestSetupTokenGuessingIsRateLimited(t *testing.T) {
	e := newEnv(t)
	var last int
	for i := 0; i < 11; i++ {
		last, _ = e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": "guess"}, nil)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("11th guess: %d, want 429", last)
	}
	// Even the right token is refused until the window passes.
	if code, _ := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, nil); code != http.StatusTooManyRequests {
		t.Errorf("right token while limited: %d, want 429", code)
	}
	e.clock.Advance(16 * time.Minute)
	if code, _ := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, nil); code != http.StatusNoContent {
		t.Errorf("right token after window: %d, want 204", code)
	}
}

func TestLoginLogout(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	if code, _ := e.call(t, "DELETE", "/api/v1/session", nil, nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("session after logout: %d, want 401", code)
	}

	code, out := e.call(t, "POST", "/api/v1/session", map[string]string{"email": "mara@example.com", "password": "wrong password!"}, nil)
	if code != http.StatusUnauthorized || out["error"] != "Email or password is incorrect." {
		t.Errorf("wrong password: %d %v", code, out)
	}
	// Unknown accounts get the identical answer.
	_, out2 := e.call(t, "POST", "/api/v1/session", map[string]string{"email": "nobody@example.com", "password": "whatever whatever"}, nil)
	if out2["error"] != out["error"] {
		t.Errorf("unknown account answer %v differs from wrong password %v", out2, out)
	}
	// Email is matched case-insensitively.
	if code, _ := e.call(t, "POST", "/api/v1/session", map[string]string{"email": "MARA@example.com", "password": owner["password"]}, nil); code != http.StatusOK {
		t.Errorf("login: %d", code)
	}
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusOK {
		t.Errorf("session after login: %d", code)
	}
}

func TestSessionsExpireWhenIdle(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	e.clock.Advance(6 * 24 * time.Hour)
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusOK {
		t.Fatalf("session after 6 idle days: %d, want 200", code)
	}
	e.clock.Advance(6 * 24 * time.Hour) // 12 days in, but the last use slid the expiry
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusOK {
		t.Fatalf("session 6 days after last use: %d, want 200", code)
	}
	e.clock.Advance(8 * 24 * time.Hour)
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("session after 8 idle days: %d, want 401", code)
	}
}

func TestBruteForceLoginIsRateLimited(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	var last int
	for i := 0; i < 11; i++ {
		last, _ = e.call(t, "POST", "/api/v1/session", map[string]string{"email": "mara@example.com", "password": "guess guess guess"}, nil)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("11th attempt on one account: %d, want 429", last)
	}
}

func TestCrossSiteRequestsAreRefused(t *testing.T) {
	e := newEnv(t)
	for _, hdr := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		code, _ := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, hdr)
		if code != http.StatusForbidden {
			t.Errorf("%v: %d, want 403", hdr, code)
		}
	}
	// The console's own origin is fine.
	code, _ := e.call(t, "POST", "/api/v1/setup/verify", map[string]string{"token": testToken}, map[string]string{"Origin": e.srv.URL})
	if code != http.StatusNoContent {
		t.Errorf("same origin: %d, want 204", code)
	}
}

func TestViewerCannotReadAuditLog(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	ctx := context.Background()
	hash := mustHash(t, "viewer password 1")
	if err := e.store.CreateUser(ctx, &store.User{Email: "sam@example.com", Name: "Sam", PasswordHash: hash, Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	e.call(t, "POST", "/api/v1/session", map[string]string{"email": "sam@example.com", "password": "viewer password 1"}, nil)
	if code, _ := e.call(t, "GET", "/api/v1/audit", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer audit: %d, want 403", code)
	}
}

func TestProductionCookiesAreHostPrefixedAndSecure(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := Handler(Config{UI: fstest.MapFS{}, Logger: slog.New(slog.DiscardHandler), Store: st,
		SetupTokens: setup.NewStaticTokenSource(testToken, time.Hour)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/setup/verify", strings.NewReader(`{"token":"`+testToken+`"}`)))
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != "__Host-kwerft_setup" || !c[0].Secure || !c[0].HttpOnly || c[0].Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
