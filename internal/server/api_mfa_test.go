package server

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/descope/virtualwebauthn"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

var testDataKey = []byte("0123456789abcdef0123456789abcdef")

// withMFA gives the console a data key and passkeys for console.test.
func withMFA(c *Config) {
	c.DataKey = testDataKey
	c.ConsoleDomain = "console.test"
	c.PasskeyOrigins = []string{"https://console.test"}
}

var testRP = virtualwebauthn.RelyingParty{Name: "Kwerft", ID: "console.test", Origin: "https://console.test"}

// newClient is a second browser: its own cookie jar against the same console.
func (e *env) newClient() *env {
	jar, _ := cookiejar.New(nil)
	c := *e
	c.client = &http.Client{Jar: jar}
	return &c
}

// raw sends a body as is and returns the response body unparsed.
func (e *env) raw(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (e *env) login(t *testing.T) (int, map[string]any) {
	t.Helper()
	return e.call(t, "POST", "/api/v1/session", map[string]string{"email": owner["email"], "password": owner["password"]}, nil)
}

func (e *env) signedIn(t *testing.T) bool {
	t.Helper()
	code, _ := e.call(t, "GET", "/api/v1/session", nil, nil)
	return code == http.StatusOK
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// enrollTOTP turns on an authenticator app for the signed-in owner and
// returns its secret and the recovery codes shown.
func (e *env) enrollTOTP(t *testing.T) ([]byte, []string) {
	t.Helper()
	code, out := e.call(t, "POST", "/api/v1/account/totp", map[string]string{"password": owner["password"]}, nil)
	if code != http.StatusOK {
		t.Fatalf("totp start: %d %v", code, out)
	}
	u, err := url.Parse(out["uri"].(string))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(u.Query().Get("secret"))
	if err != nil {
		t.Fatal(err)
	}
	code, out = e.call(t, "POST", "/api/v1/account/totp/confirm", map[string]string{"code": auth.TOTPCode(secret, e.clock.Now())}, nil)
	if code != http.StatusOK {
		t.Fatalf("totp confirm: %d %v", code, out)
	}
	var codes []string
	if out["recoveryCodes"] != nil {
		codes = strs(out["recoveryCodes"])
	}
	return secret, codes
}

func auditActions(t *testing.T, st *store.Store) []string {
	t.Helper()
	entries, err := st.RecentAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}

func TestTOTPEnrollmentNeedsPasswordAndAWorkingCode(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)

	if code, out := e.call(t, "POST", "/api/v1/account/totp", map[string]string{}, nil); code != http.StatusBadRequest || out["field"] != "password" {
		t.Errorf("no password: %d %v", code, out)
	}
	if code, out := e.call(t, "POST", "/api/v1/account/totp", map[string]string{"password": "not my password"}, nil); code != http.StatusBadRequest || out["field"] != "password" {
		t.Errorf("wrong password: %d %v", code, out)
	}
	code, out := e.call(t, "POST", "/api/v1/account/totp", map[string]string{"password": owner["password"]}, nil)
	if code != http.StatusOK || !strings.HasPrefix(out["qr"].(string), "data:image/svg+xml;base64,") ||
		!strings.HasPrefix(out["uri"].(string), "otpauth://totp/Kwerft%20console.test:mara@example.com?") {
		t.Fatalf("start: %d %v", code, out)
	}
	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(out["secret"].(string), " ", ""))

	// Not on until confirmed: signing in still needs only the password.
	if _, acc := e.call(t, "GET", "/api/v1/account", nil, nil); acc["totp"] != false {
		t.Errorf("pending enrollment reported as on: %v", acc)
	}
	// The secret is encrypted at rest.
	u, _ := e.store.UserByEmail(context.Background(), owner["email"])
	row, _ := e.store.TOTPFor(context.Background(), u.ID)
	if !strings.HasPrefix(row.Secret, "v1:") || strings.Contains(row.Secret, b32.EncodeToString(secret)) {
		t.Errorf("secret stored as %q", row.Secret)
	}

	wrong := auth.TOTPCode(secret, e.clock.Now().Add(5*time.Minute))
	if code, out := e.call(t, "POST", "/api/v1/account/totp/confirm", map[string]string{"code": wrong}, nil); code != http.StatusBadRequest || out["field"] != "code" {
		t.Errorf("wrong code: %d %v", code, out)
	}
	code, out = e.call(t, "POST", "/api/v1/account/totp/confirm", map[string]string{"code": auth.TOTPCode(secret, e.clock.Now())}, nil)
	if code != http.StatusOK || len(strs(out["recoveryCodes"])) != auth.RecoveryCodeCount {
		t.Fatalf("confirm: %d %v", code, out)
	}
	if _, acc := e.call(t, "GET", "/api/v1/account", nil, nil); acc["totp"] != true || acc["recoveryCodesLeft"] != float64(10) {
		t.Errorf("account after confirm: %v", acc)
	}
	// A second app needs the first turned off.
	if code, _ := e.call(t, "POST", "/api/v1/account/totp", map[string]string{"password": owner["password"]}, nil); code != http.StatusConflict {
		t.Errorf("second enrollment: %d, want 409", code)
	}
	if acts := auditActions(t, e.store); !slices.Contains(acts, "account.totp_enabled") || !slices.Contains(acts, "account.recovery_codes_generated") {
		t.Errorf("audit: %v", acts)
	}
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func TestLoginWithTOTP(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	secret, _ := e.enrollTOTP(t)
	e.call(t, "DELETE", "/api/v1/session", nil, nil)

	code, out := e.login(t)
	if code != http.StatusOK || !slices.Equal(strs(out["secondFactor"]), []string{"totp", "recovery"}) {
		t.Fatalf("password step: %d %v", code, out)
	}
	if e.signedIn(t) {
		t.Fatal("signed in after the password alone")
	}
	// The code that confirmed the enrollment was used: same step, refused.
	code, out = e.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": auth.TOTPCode(secret, e.clock.Now())}, nil)
	if code != http.StatusUnauthorized || out["field"] != "code" || !strings.Contains(out["error"].(string), "already used") {
		t.Errorf("reused code: %d %v", code, out)
	}
	e.clock.Advance(30 * time.Second)
	if code, out := e.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": "000000"}, nil); code != http.StatusUnauthorized || out["field"] != "code" {
		t.Errorf("wrong code: %d %v", code, out)
	}
	now := auth.TOTPCode(secret, e.clock.Now())
	code, out = e.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": now[:3] + " " + now[3:]}, nil)
	if code != http.StatusOK || out["email"] != owner["email"] {
		t.Fatalf("right code: %d %v", code, out)
	}
	if !e.signedIn(t) {
		t.Fatal("not signed in after the second factor")
	}
	// The pending sign-in is gone; a replay with another browser fails too.
	other := e.newClient()
	other.login(t)
	if code, _ := other.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": now}, nil); code != http.StatusUnauthorized {
		t.Errorf("replayed code: %d, want 401", code)
	}
	acts := auditActions(t, e.store)
	if !slices.Contains(acts, "session.second_factor_failed") || !slices.Contains(acts, "session.login") {
		t.Errorf("audit: %v", acts)
	}
}

func TestLoginWithRecoveryCodeIsSingleUse(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	_, codes := e.enrollTOTP(t)
	e.call(t, "DELETE", "/api/v1/session", nil, nil)

	e.login(t)
	if code, out := e.call(t, "POST", "/api/v1/session/second-factor/recovery", map[string]string{"code": strings.ToUpper(codes[3])}, nil); code != http.StatusOK {
		t.Fatalf("recovery code: %d %v", code, out)
	}
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	e.login(t)
	if code, out := e.call(t, "POST", "/api/v1/session/second-factor/recovery", map[string]string{"code": codes[3]}, nil); code != http.StatusUnauthorized || out["field"] != "code" {
		t.Errorf("used code again: %d %v", code, out)
	}
	if code, _ := e.call(t, "POST", "/api/v1/session/second-factor/recovery", map[string]string{"code": codes[4]}, nil); code != http.StatusOK {
		t.Errorf("another code: %d", code)
	}
	if _, acc := e.call(t, "GET", "/api/v1/account", nil, nil); acc["recoveryCodesLeft"] != float64(8) {
		t.Errorf("left: %v", acc["recoveryCodesLeft"])
	}
	if acts := auditActions(t, e.store); !slices.Contains(acts, "account.recovery_code_used") {
		t.Errorf("audit: %v", acts)
	}
	// Regenerating replaces all codes; the old ones stop working.
	code, out := e.call(t, "POST", "/api/v1/account/recovery-codes", map[string]string{"password": owner["password"]}, nil)
	if code != http.StatusOK || len(strs(out["recoveryCodes"])) != 10 {
		t.Fatalf("regenerate: %d %v", code, out)
	}
	fresh := strs(out["recoveryCodes"])
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	e.login(t)
	if code, _ := e.call(t, "POST", "/api/v1/session/second-factor/recovery", map[string]string{"code": codes[5]}, nil); code != http.StatusUnauthorized {
		t.Errorf("old code after regenerate: %d", code)
	}
	if code, _ := e.call(t, "POST", "/api/v1/session/second-factor/recovery", map[string]string{"code": fresh[0]}, nil); code != http.StatusOK {
		t.Errorf("new code: %d", code)
	}
}

func TestSecondFactorGuessingIsLimited(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	e.enrollTOTP(t)
	e.call(t, "DELETE", "/api/v1/session", nil, nil)

	e.login(t)
	var out map[string]any
	for i := 0; i < maxPendingFailures; i++ {
		_, out = e.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": "000000"}, nil)
	}
	if out["error"] != "Too many wrong attempts. Enter your password again." {
		t.Errorf("5th wrong code: %v", out)
	}
	// The pending sign-in is gone: back to the password.
	if code, out := e.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": "000000"}, nil); code != http.StatusUnauthorized || out["field"] != nil {
		t.Errorf("after limit: %d %v", code, out)
	}
	// Per account, across fresh password sign-ins: 10 per 15 minutes.
	e.login(t)
	for i := 0; i < maxPendingFailures; i++ {
		e.call(t, "POST", "/api/v1/session/second-factor/recovery", map[string]string{"code": "nope"}, nil)
	}
	e.login(t)
	last, _ := e.call(t, "POST", "/api/v1/session/second-factor/recovery", map[string]string{"code": "nope"}, nil)
	if last != http.StatusTooManyRequests {
		t.Errorf("11th second-factor attempt on the account: %d, want 429", last)
	}
}

func TestPendingLoginExpires(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	secret, _ := e.enrollTOTP(t)
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	e.login(t)
	e.clock.Advance(pendingTTL + time.Second)
	code, out := e.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": auth.TOTPCode(secret, e.clock.Now())}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(out["error"].(string), "timed out") {
		t.Errorf("after 5 minutes: %d %v", code, out)
	}
	if code, _ := e.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": "123456"}, map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("cross-site second factor: %d, want 403", code)
	}
}

func TestDisableTOTP(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	secret, _ := e.enrollTOTP(t)
	if code, _ := e.call(t, "POST", "/api/v1/account/totp/disable", map[string]string{"password": "wrong password!"}, nil); code != http.StatusBadRequest {
		t.Errorf("wrong password: %d", code)
	}
	if code, _ := e.call(t, "POST", "/api/v1/account/totp/disable", map[string]string{"password": "123456"}, nil); code != http.StatusBadRequest {
		t.Errorf("wrong code: %d", code)
	}
	// A current code works instead of the password.
	e.clock.Advance(30 * time.Second)
	if code, out := e.call(t, "POST", "/api/v1/account/totp/disable", map[string]string{"password": auth.TOTPCode(secret, e.clock.Now())}, nil); code != http.StatusNoContent {
		t.Fatalf("disable with code: %d %v", code, out)
	}
	if _, acc := e.call(t, "GET", "/api/v1/account", nil, nil); acc["totp"] != false || acc["recoveryCodesLeft"] != float64(0) {
		t.Errorf("after disable: %v", acc)
	}
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	if code, out := e.login(t); code != http.StatusOK || out["email"] != owner["email"] {
		t.Errorf("password-only login after disable: %d %v", code, out)
	}
	acts := auditActions(t, e.store)
	if !slices.Contains(acts, "account.totp_disabled") || !slices.Contains(acts, "account.confirm_failed") {
		t.Errorf("audit: %v", acts)
	}
	// Recovery codes need a second factor to stand in for.
	if code, _ := e.call(t, "POST", "/api/v1/account/recovery-codes", map[string]string{"password": owner["password"]}, nil); code != http.StatusConflict {
		t.Errorf("recovery codes without a factor: %d, want 409", code)
	}
}

func TestWrongDataKeyFailsClosed(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	secret, _ := e.enrollTOTP(t)
	// The same database behind a console started with another key.
	other := httptest.NewServer(Handler(Config{
		UI: fstest.MapFS{"index.html": {Data: []byte("ui")}}, Logger: slog.New(slog.DiscardHandler), Store: e.store, SetupTokens: e.tokens, InsecureCookies: true, Now: e.clock.Now,
		DataKey: []byte("ffffffffffffffffffffffffffffffff"),
	}))
	defer other.Close()
	o := e.newClient()
	o.srv = other
	o.login(t)
	e.clock.Advance(30 * time.Second)
	if code, _ := o.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": auth.TOTPCode(secret, e.clock.Now())}, nil); code != http.StatusInternalServerError {
		t.Errorf("wrong key: %d, want 500", code)
	}
	if o.signedIn(t) {
		t.Error("signed in although the secret could not be decrypted")
	}
}

func TestTOTPNeedsDataKey(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	code, out := e.call(t, "POST", "/api/v1/account/totp", map[string]string{"password": owner["password"]}, nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(out["error"].(string), "KWERFT_DATA_KEY") {
		t.Errorf("without data key: %d %v", code, out)
	}
	if code, _ := e.call(t, "POST", "/api/v1/session/passkey/begin", nil, nil); code != http.StatusServiceUnavailable {
		t.Errorf("passkeys without console domain: %d, want 503", code)
	}
	if _, acc := e.call(t, "GET", "/api/v1/account", nil, nil); acc["available"].(map[string]any)["totp"] != false {
		t.Errorf("available: %v", acc["available"])
	}
}

func TestChangePasswordSignsOutOtherSessions(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	other := e.newClient()
	other.login(t)
	if !other.signedIn(t) {
		t.Fatal("second browser not signed in")
	}
	if code, out := e.call(t, "POST", "/api/v1/account/password", map[string]string{"current": "wrong password!", "new": "a brand new passphrase"}, nil); code != http.StatusBadRequest || out["field"] != "current" {
		t.Errorf("wrong current: %d %v", code, out)
	}
	if code, out := e.call(t, "POST", "/api/v1/account/password", map[string]string{"current": owner["password"], "new": "short"}, nil); code != http.StatusBadRequest || out["field"] != "new" {
		t.Errorf("weak new: %d %v", code, out)
	}
	code, out := e.call(t, "POST", "/api/v1/account/password", map[string]string{"current": owner["password"], "new": "a brand new passphrase"}, nil)
	if code != http.StatusOK || out["signedOut"] != float64(1) {
		t.Fatalf("change: %d %v", code, out)
	}
	if !e.signedIn(t) || other.signedIn(t) {
		t.Error("want this session kept and the other one ended")
	}
	if code, _ := other.login(t); code != http.StatusUnauthorized {
		t.Errorf("old password: %d, want 401", code)
	}
	if acts := auditActions(t, e.store); !slices.Contains(acts, "account.password_changed") {
		t.Errorf("audit: %v", acts)
	}
}

func TestAccountSessionsAndName(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	b, c := e.newClient(), e.newClient()
	b.login(t)
	c.login(t)
	_, acc := e.call(t, "GET", "/api/v1/account", nil, nil)
	sessions := acc["sessions"].([]any)
	if len(sessions) != 3 {
		t.Fatalf("sessions: %v", sessions)
	}
	var mine, others []string
	for _, s := range sessions {
		m := s.(map[string]any)
		if m["current"] == true {
			mine = append(mine, m["id"].(string))
		} else {
			others = append(others, m["id"].(string))
		}
	}
	if len(mine) != 1 {
		t.Fatalf("current sessions: %v", mine)
	}
	if code, _ := e.call(t, "DELETE", "/api/v1/account/sessions/"+mine[0], nil, nil); code != http.StatusBadRequest {
		t.Errorf("revoke own session: %d, want 400", code)
	}
	if code, _ := e.call(t, "DELETE", "/api/v1/account/sessions/"+others[0], nil, nil); code != http.StatusNoContent {
		t.Errorf("revoke one: %d", code)
	}
	if code, out := e.call(t, "DELETE", "/api/v1/account/sessions", nil, nil); code != http.StatusOK || out["signedOut"] != float64(1) {
		t.Errorf("revoke others: %d %v", code, out)
	}
	if b.signedIn(t) || c.signedIn(t) || !e.signedIn(t) {
		t.Error("only this session should remain")
	}

	if code, out := e.call(t, "PATCH", "/api/v1/account", map[string]string{"name": "  "}, nil); code != http.StatusBadRequest || out["field"] != "name" {
		t.Errorf("empty name: %d %v", code, out)
	}
	if code, out := e.call(t, "PATCH", "/api/v1/account", map[string]string{"name": "Mara L."}, nil); code != http.StatusOK || out["name"] != "Mara L." {
		t.Errorf("rename: %d %v", code, out)
	}
	if _, out := e.call(t, "GET", "/api/v1/session", nil, nil); out["name"] != "Mara L." {
		t.Errorf("session after rename: %v", out)
	}
}

func TestUsersWithoutSecondFactorKeepPasswordLogin(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	code, out := e.login(t)
	if code != http.StatusOK || out["secondFactor"] != nil || out["email"] != owner["email"] || !e.signedIn(t) {
		t.Errorf("login: %d %v", code, out)
	}
	// Second-factor endpoints need a pending sign-in.
	for _, p := range []string{"/api/v1/session/second-factor/totp", "/api/v1/session/second-factor/passkey/begin"} {
		if code, _ := e.newClient().call(t, "POST", p, map[string]string{"code": "123456"}, nil); code != http.StatusUnauthorized {
			t.Errorf("%s without a pending sign-in: %d, want 401", p, code)
		}
	}
}

// ---- passkeys, with a virtual authenticator ---------------------------------

// addPasskey registers a new credential on authenticator and returns the
// finish response.
func (e *env) addPasskey(t *testing.T, authenticator *virtualwebauthn.Authenticator, name string) (int, map[string]any) {
	t.Helper()
	code, opts := e.raw(t, "POST", "/api/v1/account/passkeys/begin", `{"password":"`+owner["password"]+`"}`)
	if code != http.StatusOK {
		t.Fatalf("begin: %d %s", code, opts)
	}
	parsed, err := virtualwebauthn.ParseAttestationOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	resp := virtualwebauthn.CreateAttestationResponse(testRP, *authenticator, cred, *parsed)
	body, _ := json.Marshal(map[string]any{"name": name, "credential": json.RawMessage(resp)})
	code, raw := e.raw(t, "POST", "/api/v1/account/passkeys/finish", string(body))
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	if code == http.StatusCreated {
		authenticator.AddCredential(cred)
	}
	return code, out
}

// assert answers a passkey prompt from begin with the authenticator.
func (e *env) assert(t *testing.T, authenticator virtualwebauthn.Authenticator, begin, finish string) (int, map[string]any) {
	t.Helper()
	code, opts := e.raw(t, "POST", begin, "")
	if code != http.StatusOK {
		t.Fatalf("%s: %d %s", begin, code, opts)
	}
	parsed, err := virtualwebauthn.ParseAssertionOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	cred := authenticator.FindAllowedCredential(*parsed)
	if len(parsed.AllowCredentials) == 0 && len(authenticator.Credentials) > 0 {
		cred = &authenticator.Credentials[0] // discoverable: the authenticator picks
	}
	if cred == nil {
		t.Fatalf("no credential allowed by %s", opts)
	}
	cred.Counter++
	resp := virtualwebauthn.CreateAssertionResponse(testRP, authenticator, *cred, *parsed)
	code, raw := e.raw(t, "POST", finish, resp)
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return code, out
}

func TestPasskeysEndToEnd(t *testing.T) {
	e := newEnv(t, withMFA)
	ownerOut := e.completeSetup(t)
	userID := ownerOut["id"].(string)
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: []byte(userID)})

	// Registration asks for the password first.
	if code, _ := e.raw(t, "POST", "/api/v1/account/passkeys/begin", `{"password":"nope nope nope"}`); code != http.StatusBadRequest {
		t.Errorf("begin with wrong password: %d", code)
	}
	code, out := e.addPasskey(t, &authenticator, "MacBook")
	if code != http.StatusCreated || out["passkey"].(map[string]any)["name"] != "MacBook" || len(strs(out["recoveryCodes"])) != 10 {
		t.Fatalf("register: %d %v", code, out)
	}
	// A second passkey does not reset the recovery codes.
	if code, out := e.addPasskey(t, &authenticator, ""); code != http.StatusCreated || out["recoveryCodes"] != nil || out["passkey"].(map[string]any)["name"] != "Passkey" {
		t.Fatalf("second: %d %v", code, out)
	}
	// Finishing without a begin fails.
	if code, _ := e.raw(t, "POST", "/api/v1/account/passkeys/finish", `{"name":"x","credential":{}}`); code != http.StatusConflict {
		t.Errorf("finish without begin: %d, want 409", code)
	}

	// Password, then passkey.
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	code, out = e.login(t)
	if code != http.StatusOK || !slices.Equal(strs(out["secondFactor"]), []string{"passkey", "recovery"}) {
		t.Fatalf("password step: %d %v", code, out)
	}
	code, out = e.assert(t, authenticator, "/api/v1/session/second-factor/passkey/begin", "/api/v1/session/second-factor/passkey/finish")
	if code != http.StatusOK || out["email"] != owner["email"] || !e.signedIn(t) {
		t.Fatalf("passkey second factor: %d %v", code, out)
	}

	// Passwordless, from a fresh browser.
	fresh := e.newClient()
	code, out = fresh.assert(t, authenticator, "/api/v1/session/passkey/begin", "/api/v1/session/passkey/finish")
	if code != http.StatusOK || out["email"] != owner["email"] || !fresh.signedIn(t) {
		t.Fatalf("passwordless: %d %v", code, out)
	}
	// The ceremony cookie is single use.
	if code, _ := fresh.raw(t, "POST", "/api/v1/session/passkey/finish", "{}"); code != http.StatusUnauthorized {
		t.Errorf("finish again: %d, want 401", code)
	}

	// An authenticator the console has never seen is refused.
	stranger := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: []byte(userID)})
	stranger.AddCredential(virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2))
	if code, _ := e.newClient().assert(t, stranger, "/api/v1/session/passkey/begin", "/api/v1/session/passkey/finish"); code != http.StatusUnauthorized {
		t.Errorf("unknown passkey: %d, want 401", code)
	}

	// List, rename, remove.
	_, acc := e.call(t, "GET", "/api/v1/account", nil, nil)
	keys := acc["passkeys"].([]any)
	if len(keys) != 2 || keys[0].(map[string]any)["lastUsedAt"] == nil {
		t.Fatalf("passkeys: %v", keys)
	}
	first := keys[0].(map[string]any)["id"].(string)
	if code, _ := e.call(t, "PATCH", "/api/v1/account/passkeys/"+first, map[string]string{"name": "Work laptop"}, nil); code != http.StatusNoContent {
		t.Errorf("rename: %d", code)
	}
	if code, _ := e.call(t, "PATCH", "/api/v1/account/passkeys/nope", map[string]string{"name": "x"}, nil); code != http.StatusNotFound {
		t.Errorf("rename unknown: %d", code)
	}
	if code, _ := e.call(t, "DELETE", "/api/v1/account/passkeys/"+first, map[string]string{}, nil); code != http.StatusBadRequest {
		t.Errorf("remove without password: %d, want 400", code)
	}
	for _, k := range keys {
		id := k.(map[string]any)["id"].(string)
		if code, _ := e.call(t, "DELETE", "/api/v1/account/passkeys/"+id, map[string]string{"password": owner["password"]}, nil); code != http.StatusNoContent {
			t.Errorf("remove: %d", code)
		}
	}
	// With the last factor gone, so are the recovery codes and the second step.
	if _, acc := e.call(t, "GET", "/api/v1/account", nil, nil); acc["recoveryCodesLeft"] != float64(0) || len(acc["passkeys"].([]any)) != 0 {
		t.Errorf("after removing all: %v", acc)
	}
	e.call(t, "DELETE", "/api/v1/session", nil, nil)
	if code, out := e.login(t); code != http.StatusOK || out["secondFactor"] != nil {
		t.Errorf("login after removing passkeys: %d %v", code, out)
	}
	acts := auditActions(t, e.store)
	for _, want := range []string{"account.passkey_added", "account.passkey_renamed", "account.passkey_removed", "session.login_failed"} {
		if !slices.Contains(acts, want) {
			t.Errorf("audit lacks %s: %v", want, acts)
		}
	}
}

func TestPasskeyBeginEndpoints(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	code, raw := e.newClient().raw(t, "POST", "/api/v1/session/passkey/begin", "")
	var opts struct {
		PublicKey struct {
			Challenge        string `json:"challenge"`
			RPID             string `json:"rpId"`
			UserVerification string `json:"userVerification"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(raw), &opts); code != http.StatusOK || err != nil || opts.PublicKey.Challenge == "" ||
		opts.PublicKey.RPID != "console.test" || opts.PublicKey.UserVerification != "required" {
		t.Errorf("passwordless begin: %d %s", code, raw)
	}
	code, raw = e.raw(t, "POST", "/api/v1/account/passkeys/begin", `{"password":"`+owner["password"]+`"}`)
	if code != http.StatusOK || !strings.Contains(raw, `"residentKey":"preferred"`) || !strings.Contains(raw, `"name":"Kwerft"`) {
		t.Errorf("register begin: %d %s", code, raw)
	}
}
