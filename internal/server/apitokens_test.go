package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

// newAPIToken creates a token as the signed-in user and returns it.
func (e *env) newAPIToken(t *testing.T, body map[string]any) (string, map[string]any) {
	t.Helper()
	code, out := e.call(t, "POST", "/api/v1/account/tokens", body, nil)
	if code != http.StatusCreated {
		t.Fatalf("create token %v: %d %v", body, code, out)
	}
	tok, _ := out["token"].(string)
	if !auth.LooksLikeAPIToken(tok) {
		t.Fatalf("token %q", tok)
	}
	return tok, out["apiToken"].(map[string]any)
}

// bearer is a client without cookies that sends the token.
func (e *env) bearer(t *testing.T, tok, method, path string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	h := map[string]string{"Authorization": "Bearer " + tok}
	for i := 0; i+1 < len(hdr); i += 2 {
		h[hdr[i]] = hdr[i+1]
	}
	return e.newClient().call(t, method, path, body, h)
}

func lastAudit(t *testing.T, st *store.Store, action string) *store.AuditEntry {
	t.Helper()
	list, err := st.RecentAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for i := range list {
		if list[i].Action == action {
			return &list[i]
		}
	}
	return nil
}

func TestAPITokenAuthenticatesAndIsShownOnce(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	tok, info := e.newAPIToken(t, map[string]any{"name": "ci deploy", "role": "viewer", "expiresInDays": 30})
	if info["role"] != "viewer" || info["kind"] != "api" || !strings.HasPrefix(tok, strings.TrimSuffix(info["hint"].(string), "…")) {
		t.Errorf("token info %v", info)
	}
	// Stored as a hash only.
	u := mustUser(t, e.store, owner["email"])
	list, _ := e.store.APITokens(context.Background(), u.ID)
	if len(list) != 1 || list[0].TokenHash != auth.HashToken(tok) || strings.Contains(list[0].TokenHash, tok) {
		t.Fatalf("stored %+v", list)
	}
	// The list never shows it again.
	code, out := e.call(t, "GET", "/api/v1/account/tokens", nil, nil)
	if code != http.StatusOK || strings.Contains(stringify(out), tok[5:]) || len(out["tokens"].([]any)) != 1 {
		t.Errorf("list: %d %v", code, out)
	}
	// It authenticates, with the capped role.
	code, out = e.bearer(t, tok, "GET", "/api/v1/session", nil)
	if code != http.StatusOK || out["email"] != owner["email"] || out["role"] != "viewer" {
		t.Errorf("session by token: %d %v", code, out)
	}
	// Last use recorded, with the client's address.
	list, _ = e.store.APITokens(context.Background(), u.ID)
	if list[0].LastUsedAt.IsZero() || list[0].LastUsedIP != "127.0.0.1" {
		t.Errorf("last use %+v", list[0])
	}
	if a := lastAudit(t, e.store, "token.created"); a == nil || a.Target != "ci deploy" || !strings.Contains(a.Detail, "viewer") {
		t.Errorf("audit %+v", a)
	}
	// Expiry: default 90 days, at most a year.
	_, info = e.newAPIToken(t, map[string]any{"name": "default"})
	exp, _ := time.Parse(time.RFC3339, info["expiresAt"].(string))
	if d := exp.Sub(e.clock.Now()); d < 89*24*time.Hour || d > 91*24*time.Hour || info["role"] != "owner" {
		t.Errorf("default expiry %v, role %v", d, info["role"])
	}
	if code, out := e.call(t, "POST", "/api/v1/account/tokens", map[string]any{"name": "x", "expiresInDays": 366}, nil); code != http.StatusBadRequest || out["field"] != "expiresInDays" {
		t.Errorf("366 days: %d %v", code, out)
	}
}

func stringify(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				b.WriteString(k + "=")
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		case string:
			b.WriteString(x + ";")
		}
	}
	walk(v)
	return b.String()
}

// Privilege escalation through tokens: never above the user's role, never
// past a demotion, never into identity administration.
func TestAPITokenCannotEscalate(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	dev, devID := e.member(t, "dev@example.com", store.RoleDeveloper)

	// A developer cannot mint an admin or owner token.
	for _, role := range []string{"admin", "owner", "root"} {
		if code, out := dev.call(t, "POST", "/api/v1/account/tokens", map[string]any{"name": "x", "role": role}, nil); code != http.StatusBadRequest || out["field"] != "role" {
			t.Errorf("developer → %s token: %d %v", role, code, out)
		}
	}
	// An owner's token capped to viewer is a viewer.
	ownerViewer, _ := e.newAPIToken(t, map[string]any{"name": "read only", "role": "viewer"})
	if code, _ := e.bearer(t, ownerViewer, "GET", "/api/v1/members", nil); code != http.StatusForbidden {
		t.Errorf("viewer-capped owner token reads members: %d", code)
	}
	// An owner token follows its user's demotion at once.
	ownerTok, _ := e.newAPIToken(t, map[string]any{"name": "full"})
	if code, _ := e.bearer(t, ownerTok, "GET", "/api/v1/audit", nil); code != http.StatusOK {
		t.Fatalf("owner token reads audit: %d", code)
	}
	devTok, _ := dev.newAPIToken(t, map[string]any{"name": "dev"})
	if code, _ := e.call(t, "PATCH", "/api/v1/members/"+devID, map[string]string{"role": "viewer"}, nil); code != http.StatusOK {
		t.Fatal("demote")
	}
	if code, out := e.bearer(t, devTok, "GET", "/api/v1/session", nil); code != http.StatusOK || out["role"] != "viewer" {
		t.Errorf("token after demotion: %d %v", code, out)
	}
	// Tokens manage no identity: not tokens (no minting a longer-lived one),
	// not the account, members, invites, sign-in settings or the data key.
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/account/tokens"},
		{"POST", "/api/v1/account/tokens"},
		{"POST", "/api/v1/account/kubeconfig"},
		{"GET", "/api/v1/account"},
		{"POST", "/api/v1/account/password"},
		{"DELETE", "/api/v1/account/sessions"},
		{"POST", "/api/v1/account/sso/unlink"},
		{"DELETE", "/api/v1/session"},
		{"GET", "/api/v1/members"},
		{"POST", "/api/v1/invites"},
		{"GET", "/api/v1/settings/data-key"},
		{"POST", "/api/v1/settings/data-key/rotate"},
	} {
		code, out := e.bearer(t, ownerTok, c.method, c.path, map[string]any{"name": "x", "password": owner["password"]})
		if code != http.StatusForbidden {
			t.Errorf("%s %s with a token: %d %v", c.method, c.path, code, out)
		}
	}
	if a := lastAudit(t, e.store, "token.denied"); a == nil || a.Actor != owner["email"] {
		t.Errorf("denials not audited: %+v", a)
	}
	// Removing the member removes their tokens.
	if code, _ := e.call(t, "DELETE", "/api/v1/members/"+devID, nil, nil); code != http.StatusNoContent {
		t.Fatal("remove")
	}
	if code, _ := e.bearer(t, devTok, "GET", "/api/v1/session", nil); code != http.StatusUnauthorized {
		t.Errorf("token of a removed member: %d", code)
	}
}

func TestAPITokenRevokeExpiryAndCookies(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	tok, info := e.newAPIToken(t, map[string]any{"name": "short", "expiresInDays": 1})
	// A bad token is refused even when a valid session cookie comes along:
	// with an Authorization header, cookies are not looked at.
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, map[string]string{"Authorization": "Bearer kwft_" + strings.Repeat("A", 43)}); code != http.StatusUnauthorized {
		t.Errorf("bad token + cookie: %d", code)
	}
	if code, _ := e.call(t, "GET", "/api/v1/session", nil, map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}); code != http.StatusUnauthorized {
		t.Errorf("basic auth + cookie: %d", code)
	}
	if a := lastAudit(t, e.store, "token.rejected"); a == nil || a.Actor != "anonymous" || strings.Contains(a.Target, strings.Repeat("A", 20)) {
		t.Errorf("rejection audit %+v", a)
	}
	// Expired.
	e.clock.Advance(25 * time.Hour)
	if code, out := e.bearer(t, tok, "GET", "/api/v1/session", nil); code != http.StatusUnauthorized || !strings.Contains(out["error"].(string), "expired") {
		t.Errorf("expired: %d %v", code, out)
	}
	if a := lastAudit(t, e.store, "token.rejected"); a == nil || a.Actor != owner["email"] || a.Target != "short" {
		t.Errorf("expiry audit %+v", a)
	}
	// Revoked.
	tok2, info2 := e.newAPIToken(t, map[string]any{"name": "revoke me"})
	if code, _ := e.call(t, "DELETE", "/api/v1/account/tokens/"+info2["id"].(string), nil, nil); code != http.StatusNoContent {
		t.Fatal("revoke")
	}
	if code, _ := e.bearer(t, tok2, "GET", "/api/v1/session", nil); code != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", code)
	}
	if a := lastAudit(t, e.store, "token.revoked"); a == nil || a.Target != "revoke me" {
		t.Errorf("revoke audit %+v", a)
	}
	// Someone else's token cannot be revoked by ID.
	other, _ := e.member(t, "other@example.com", store.RoleAdmin)
	if code, _ := other.call(t, "DELETE", "/api/v1/account/tokens/"+info["id"].(string), nil, nil); code != http.StatusNotFound {
		t.Errorf("revoke another user's token: %d", code)
	}
}

// Bearer requests skip the same-origin check (nothing ambient to forge) but
// cookie requests still get it.
func TestAPITokenSkipsSameOriginButCookiesDoNot(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	tok, _ := e.newAPIToken(t, map[string]any{"name": "ci"})
	evil := map[string]string{"Origin": "https://evil.example"}
	if code, _ := e.call(t, "POST", "/api/v1/projects", map[string]string{"name": "x"}, evil); code != http.StatusForbidden {
		t.Errorf("cross-site cookie write: %d", code)
	}
	// Authenticated and past the origin check: the workload API then says
	// it has no cluster.
	if code, out := e.bearer(t, tok, "POST", "/api/v1/projects", map[string]string{"name": "x"}, "Origin", "https://evil.example"); code != http.StatusServiceUnavailable {
		t.Errorf("token write: %d %v", code, out)
	}
}

func TestAPITokenProjectRestriction(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	// Only developer or viewer tokens can be limited to projects.
	if code, out := e.call(t, "POST", "/api/v1/account/tokens", map[string]any{"name": "x", "projects": []string{"shop"}}, nil); code != http.StatusBadRequest || out["field"] != "role" {
		t.Errorf("owner token limited to projects: %d %v", code, out)
	}
	if code, out := e.call(t, "POST", "/api/v1/account/tokens", map[string]any{"name": "x", "role": "viewer", "projects": []string{"Bad Name"}}, nil); code != http.StatusBadRequest || out["field"] != "projects" {
		t.Errorf("bad project name: %d %v", code, out)
	}
	tok, info := e.newAPIToken(t, map[string]any{"name": "shop ci", "role": "developer", "projects": []string{"shop", "shop", " "}})
	if p := info["projects"].([]any); len(p) != 1 || p[0] != "shop" {
		t.Errorf("projects %v", p)
	}
	// Its project: allowed (no cluster here, so 503 after authorization).
	if code, _ := e.bearer(t, tok, "GET", "/api/v1/projects/shop/apps/web", nil); code != http.StatusServiceUnavailable {
		t.Errorf("own project: %d", code)
	}
	// Another project, and lists across projects: refused by the console.
	for _, path := range []string{"/api/v1/projects/blog/apps/web", "/api/v1/apps", "/api/v1/projects", "/api/v1/logs", "/api/v1/alerts", "/api/v1/settings"} {
		if code, out := e.bearer(t, tok, "GET", path, nil); code != http.StatusForbidden {
			t.Errorf("GET %s: %d %v", path, code, out)
		}
	}
	if code, _ := e.bearer(t, tok, "GET", "/api/v1/session", nil); code != http.StatusOK {
		t.Errorf("session: %d", code)
	}
}

func TestAPITokenFailuresAreRateLimited(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	tok, _ := e.newAPIToken(t, map[string]any{"name": "ci"})
	bad := "kwft_" + auth.NewToken()
	for i := 0; i < 20; i++ {
		if code, _ := e.bearer(t, bad, "GET", "/api/v1/session", nil); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	// The address is blocked now, valid token or not.
	if code, _ := e.bearer(t, tok, "GET", "/api/v1/session", nil); code != http.StatusTooManyRequests {
		t.Errorf("after 20 failures: %d", code)
	}
	e.clock.Advance(16 * time.Minute)
	if code, _ := e.bearer(t, tok, "GET", "/api/v1/session", nil); code != http.StatusOK {
		t.Errorf("after the window: %d", code)
	}
}

func TestKubeconfigNeedsACluster(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	if code, _ := e.call(t, "POST", "/api/v1/account/kubeconfig", map[string]any{}, nil); code != http.StatusServiceUnavailable {
		t.Errorf("kubeconfig without a cluster: %d", code)
	}
}

func TestKubeconfigYAML(t *testing.T) {
	cfg, name := kubeconfigYAML("https://ops.example.com", "ops.example.com", "mara@example.com", "kwft_x", []string{"shop", "blog"})
	for _, want := range []string{"server: https://ops.example.com/k8s", "token: kwft_x", "namespace: shop", "current-context: kwerft-ops.example.com",
		"name: mara@example.com@kwerft-ops.example.com"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("kubeconfig lacks %q:\n%s", want, cfg)
		}
	}
	if name != "kubeconfig-ops.example.com.yaml" {
		t.Errorf("filename %q", name)
	}
}

func TestEffectiveRole(t *testing.T) {
	for _, c := range []struct{ token, user, want string }{
		{"owner", "owner", "owner"}, {"viewer", "owner", "viewer"}, {"owner", "developer", "developer"},
		{"admin", "viewer", "viewer"}, {"developer", "admin", "developer"},
	} {
		if got := effectiveRole(c.token, c.user); got != c.want {
			t.Errorf("effectiveRole(%s, %s) = %s", c.token, c.user, got)
		}
	}
}
