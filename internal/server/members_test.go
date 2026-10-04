package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/store"
)

// member creates a user with role and returns a client signed in as them.
func (e *env) member(t *testing.T, email, role string) (*env, string) {
	t.Helper()
	const pw = "member password 1"
	u := &store.User{Email: email, Name: strings.Split(email, "@")[0], PasswordHash: mustHash(t, pw), Role: role}
	if err := e.store.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	c := e.newClient()
	if code, out := c.call(t, "POST", "/api/v1/session", map[string]string{"email": email, "password": pw}, nil); code != http.StatusOK {
		t.Fatalf("sign in %s: %d %v", email, code, out)
	}
	return c, u.ID
}

// invite creates an invite as the signed-in user and returns its ID and token.
func (e *env) invite(t *testing.T, email, role string) (id, token string) {
	t.Helper()
	code, out := e.call(t, "POST", "/api/v1/invites", map[string]string{"email": email, "role": role}, nil)
	if code != http.StatusCreated {
		t.Fatalf("invite %s as %s: %d %v", email, role, code, out)
	}
	url := out["url"].(string)
	i := strings.Index(url, "/invite/")
	if i < 0 {
		t.Fatalf("invite url %q", url)
	}
	return out["invite"].(map[string]any)["id"].(string), url[i+len("/invite/"):]
}

func accept(name, token string) map[string]string {
	return map[string]string{"token": token, "name": name, "password": "a long new password"}
}

// list decodes a JSON array response.
func (e *env) list(t *testing.T, path string) (int, []map[string]any) {
	t.Helper()
	code, body := e.raw(t, "GET", path, "")
	var out []map[string]any
	_ = jsonUnmarshal(body, &out)
	return code, out
}

func TestInviteIsAcceptedOnceAndSignsIn(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	_, token := e.invite(t, "sam@example.com", store.RoleDeveloper)

	guest := e.newClient()
	code, out := guest.call(t, "POST", "/api/v1/invite/lookup", map[string]string{"token": token}, nil)
	if code != http.StatusOK || out["email"] != "sam@example.com" || out["role"] != "developer" || out["invitedBy"] != owner["name"] {
		t.Fatalf("lookup: %d %v", code, out)
	}
	if code, out := guest.call(t, "POST", "/api/v1/invite/accept", map[string]string{"token": token, "name": "Sam", "password": "short"}, nil); code != http.StatusBadRequest || out["field"] != "password" {
		t.Errorf("short password: %d %v", code, out)
	}
	if code, out := guest.call(t, "POST", "/api/v1/invite/accept", map[string]string{"token": token, "name": " ", "password": "a long new password"}, nil); code != http.StatusBadRequest || out["field"] != "name" {
		t.Errorf("no name: %d %v", code, out)
	}
	code, out = guest.call(t, "POST", "/api/v1/invite/accept", accept("Sam Okafor", token), nil)
	if code != http.StatusCreated || out["role"] != "developer" || out["email"] != "sam@example.com" {
		t.Fatalf("accept: %d %v", code, out)
	}
	// Signed in straight away, as the invited role.
	if code, out := guest.call(t, "GET", "/api/v1/session", nil, nil); code != http.StatusOK || out["name"] != "Sam Okafor" || out["role"] != "developer" {
		t.Errorf("session after accept: %d %v", code, out)
	}
	// And can sign in again with the password chosen.
	again := e.newClient()
	if code, _ := again.call(t, "POST", "/api/v1/session", map[string]string{"email": "sam@example.com", "password": "a long new password"}, nil); code != http.StatusOK {
		t.Errorf("sign in with the new password: %d", code)
	}

	// Single use.
	other := e.newClient()
	if code, out := other.call(t, "POST", "/api/v1/invite/accept", accept("Mallory", token), nil); code != http.StatusGone {
		t.Errorf("second accept: %d %v", code, out)
	}
	if code, out := other.call(t, "POST", "/api/v1/invite/lookup", map[string]string{"token": token}, nil); code != http.StatusGone || !strings.Contains(out["error"].(string), "already used") {
		t.Errorf("lookup after accept: %d %v", code, out)
	}
	if _, invites := e.list(t, "/api/v1/invites"); len(invites) != 0 {
		t.Errorf("accepted invite still listed: %v", invites)
	}
	actions := auditActions(t, e.store)
	if !slices.Contains(actions, "member.invite") || !slices.Contains(actions, "member.invite_accepted") {
		t.Errorf("audit: %v", actions)
	}
}

func TestConcurrentAcceptsCreateOneAccount(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	_, token := e.invite(t, "sam@example.com", store.RoleViewer)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = e.newClient().call(t, "POST", "/api/v1/invite/accept", accept(fmt.Sprint("Sam ", i), token), nil)
		}()
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else if c != http.StatusGone {
			t.Errorf("racing accept: %d", c)
		}
	}
	if created != 1 {
		t.Errorf("%d accepts succeeded (%v), want 1", created, codes)
	}
}

func TestInviteExpiresAndCanBeReissued(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	id, token := e.invite(t, "sam@example.com", store.RoleViewer)
	e.clock.Advance(6 * 24 * time.Hour)
	e.signedIn(t) // keeps the owner's session from idling out
	e.clock.Advance(24*time.Hour + time.Minute)

	guest := e.newClient()
	if code, out := guest.call(t, "POST", "/api/v1/invite/accept", accept("Sam", token), nil); code != http.StatusGone || !strings.Contains(out["error"].(string), "expired") {
		t.Errorf("expired accept: %d %v", code, out)
	}
	_, invites := e.list(t, "/api/v1/invites")
	if len(invites) != 1 || invites[0]["state"] != "expired" {
		t.Fatalf("invites: %v", invites)
	}

	code, out := e.call(t, "POST", "/api/v1/invites/"+id+"/reissue", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("reissue: %d %v", code, out)
	}
	fresh := out["url"].(string)[strings.Index(out["url"].(string), "/invite/")+len("/invite/"):]
	if fresh == token {
		t.Fatal("reissue kept the token")
	}
	if code, _ := guest.call(t, "POST", "/api/v1/invite/lookup", map[string]string{"token": token}, nil); code != http.StatusNotFound {
		t.Errorf("old link after reissue: %d, want 404", code)
	}
	if code, out := guest.call(t, "POST", "/api/v1/invite/accept", accept("Sam", fresh), nil); code != http.StatusCreated || out["role"] != "viewer" {
		t.Errorf("accept reissued: %d %v", code, out)
	}
	if !slices.Contains(auditActions(t, e.store), "member.invite_reissued") {
		t.Error("reissue not audited")
	}
}

func TestRevokedInviteStopsWorking(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	id, token := e.invite(t, "sam@example.com", store.RoleViewer)
	if code, _ := e.call(t, "DELETE", "/api/v1/invites/"+id, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if code, out := e.newClient().call(t, "POST", "/api/v1/invite/accept", accept("Sam", token), nil); code != http.StatusGone || !strings.Contains(out["error"].(string), "withdrawn") {
		t.Errorf("accept revoked: %d %v", code, out)
	}
	if code, _ := e.call(t, "DELETE", "/api/v1/invites/"+id, nil, nil); code != http.StatusNotFound {
		t.Errorf("revoke twice: %d, want 404", code)
	}
	if code, _ := e.call(t, "POST", "/api/v1/invites/"+id+"/reissue", nil, nil); code != http.StatusNotFound {
		t.Errorf("reissue revoked: %d, want 404", code)
	}
	// A revoked invite does not block a new one.
	e.invite(t, "sam@example.com", store.RoleDeveloper)
	if !slices.Contains(auditActions(t, e.store), "member.invite_revoked") {
		t.Error("revoke not audited")
	}
}

func TestInviteRefusesExistingAccountsAndDuplicates(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	if code, out := e.call(t, "POST", "/api/v1/invites", map[string]string{"email": "MARA@example.com", "role": "viewer"}, nil); code != http.StatusConflict || out["field"] != "email" {
		t.Errorf("invite an existing account: %d %v", code, out)
	}
	if code, out := e.call(t, "POST", "/api/v1/invites", map[string]string{"email": "nope", "role": "viewer"}, nil); code != http.StatusBadRequest || out["field"] != "email" {
		t.Errorf("bad email: %d %v", code, out)
	}
	if code, out := e.call(t, "POST", "/api/v1/invites", map[string]string{"email": "a@example.com", "role": "root"}, nil); code != http.StatusBadRequest || out["field"] != "role" {
		t.Errorf("bad role: %d %v", code, out)
	}
	_, token := e.invite(t, "sam@example.com", store.RoleViewer)
	if code, out := e.call(t, "POST", "/api/v1/invites", map[string]string{"email": "Sam@example.com", "role": "admin"}, nil); code != http.StatusConflict || !strings.Contains(out["error"].(string), "open invite") {
		t.Errorf("second open invite: %d %v", code, out)
	}
	// The address got an account some other way meanwhile: the invite refuses.
	if err := e.store.CreateUser(context.Background(), &store.User{Email: "sam@example.com", Name: "Sam", PasswordHash: mustHash(t, "x long password"), Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	guest := e.newClient()
	if code, out := guest.call(t, "POST", "/api/v1/invite/lookup", map[string]string{"token": token}, nil); code != http.StatusConflict || !strings.Contains(out["error"].(string), "already has an account") {
		t.Errorf("lookup for an existing account: %d %v", code, out)
	}
	if code, _ := guest.call(t, "POST", "/api/v1/invite/accept", accept("Sam", token), nil); code != http.StatusConflict {
		t.Errorf("accept for an existing account: %d, want 409", code)
	}
}

func TestInviteTokenGuessingIsRateLimited(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	_, token := e.invite(t, "sam@example.com", store.RoleViewer)
	guest := e.newClient()
	for i := 0; i < inviteGuesses; i++ {
		if code, _ := guest.call(t, "POST", "/api/v1/invite/lookup", map[string]string{"token": fmt.Sprint("guess-", i)}, nil); code != http.StatusNotFound {
			t.Fatalf("guess %d: %d, want 404", i, code)
		}
	}
	if code, _ := guest.call(t, "POST", "/api/v1/invite/accept", accept("Sam", "guess"), nil); code != http.StatusTooManyRequests {
		t.Errorf("guess over the limit: %d, want 429", code)
	}
	// Even the right token waits for the window.
	if code, _ := guest.call(t, "POST", "/api/v1/invite/lookup", map[string]string{"token": token}, nil); code != http.StatusTooManyRequests {
		t.Errorf("right token while limited: %d, want 429", code)
	}
	e.clock.Advance(16 * time.Minute)
	if code, _ := guest.call(t, "POST", "/api/v1/invite/lookup", map[string]string{"token": token}, nil); code != http.StatusOK {
		t.Errorf("right token after the window: %d, want 200", code)
	}
}

func TestAdminsCannotInviteChangeOrRemoveOwners(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	ownerID := mustUser(t, e.store, owner["email"]).ID
	ownerInvite, _ := e.invite(t, "second-owner@example.com", store.RoleOwner)
	admin, _ := e.member(t, "ada@example.com", store.RoleAdmin)
	_, devID := e.member(t, "dev@example.com", store.RoleDeveloper)

	if code, out := admin.call(t, "POST", "/api/v1/invites", map[string]string{"email": "boss@example.com", "role": "owner"}, nil); code != http.StatusForbidden || out["field"] != "role" {
		t.Errorf("admin invites an owner: %d %v", code, out)
	}
	if code, _ := admin.call(t, "PATCH", "/api/v1/members/"+ownerID, map[string]string{"role": "admin"}, nil); code != http.StatusForbidden {
		t.Errorf("admin demotes the owner: %d, want 403", code)
	}
	if code, _ := admin.call(t, "PATCH", "/api/v1/members/"+devID, map[string]string{"role": "owner"}, nil); code != http.StatusForbidden {
		t.Errorf("admin makes an owner: %d, want 403", code)
	}
	if code, _ := admin.call(t, "DELETE", "/api/v1/members/"+ownerID, nil, nil); code != http.StatusForbidden {
		t.Errorf("admin removes the owner: %d, want 403", code)
	}
	for _, req := range []struct{ method, path string }{
		{"POST", "/api/v1/invites/" + ownerInvite + "/reissue"},
		{"DELETE", "/api/v1/invites/" + ownerInvite},
	} {
		if code, _ := admin.call(t, req.method, req.path, nil, nil); code != http.StatusForbidden {
			t.Errorf("admin %s owner invite: %d, want 403", req.method, code)
		}
	}
	if u := mustUser(t, e.store, owner["email"]); u.Role != store.RoleOwner {
		t.Fatalf("owner is now %s", u.Role)
	}

	// Everyone else is fair game for an admin.
	admin.invite(t, "viewer@example.com", store.RoleViewer)
	if code, out := admin.call(t, "PATCH", "/api/v1/members/"+devID, map[string]string{"role": "viewer"}, nil); code != http.StatusOK || out["role"] != "viewer" {
		t.Errorf("admin demotes a developer: %d %v", code, out)
	}
	_, members := admin.list(t, "/api/v1/members")
	for _, m := range members {
		if want := m["role"] != "owner"; m["manageable"] != want {
			t.Errorf("member %v: manageable = %v, want %v", m["email"], m["manageable"], want)
		}
	}

	// Developers and viewers manage nobody.
	dev, _ := e.member(t, "dev2@example.com", store.RoleDeveloper)
	for _, req := range []struct{ method, path string }{
		{"GET", "/api/v1/members"}, {"GET", "/api/v1/invites"},
		{"POST", "/api/v1/invites"}, {"PATCH", "/api/v1/members/" + devID}, {"DELETE", "/api/v1/members/" + devID},
	} {
		if code, _ := dev.call(t, req.method, req.path, map[string]string{"email": "x@example.com", "role": "admin"}, nil); code != http.StatusForbidden {
			t.Errorf("developer %s %s: %d, want 403", req.method, req.path, code)
		}
	}
}

func TestLastOwnerCannotBeDemotedOrRemoved(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	ownerID := mustUser(t, e.store, owner["email"]).ID
	if code, out := e.call(t, "PATCH", "/api/v1/members/"+ownerID, map[string]string{"role": "admin"}, nil); code != http.StatusConflict {
		t.Errorf("demote the last owner: %d %v", code, out)
	}
	if code, _ := e.call(t, "DELETE", "/api/v1/members/"+ownerID, nil, nil); code != http.StatusConflict {
		t.Errorf("remove the last owner (yourself): %d, want 409", code)
	}

	// With a second owner, the first may step down.
	second, secondID := e.member(t, "jonas@example.com", store.RoleAdmin)
	if code, _ := e.call(t, "PATCH", "/api/v1/members/"+secondID, map[string]string{"role": "owner"}, nil); code != http.StatusOK {
		t.Fatalf("promote to owner: %d", code)
	}
	if code, out := e.call(t, "PATCH", "/api/v1/members/"+ownerID, map[string]string{"role": "admin"}, nil); code != http.StatusOK || out["role"] != "admin" {
		t.Fatalf("step down with another owner: %d %v", code, out)
	}
	// Now the second is the last owner.
	if code, _ := second.call(t, "DELETE", "/api/v1/members/"+secondID, nil, nil); code != http.StatusConflict {
		t.Errorf("last owner removes themself: %d, want 409", code)
	}
	if code, _ := second.call(t, "PATCH", "/api/v1/members/"+secondID, map[string]string{"role": "viewer"}, nil); code != http.StatusConflict {
		t.Errorf("last owner demotes themself: %d, want 409", code)
	}
	if n, _ := e.store.CountOwners(context.Background()); n != 1 {
		t.Errorf("%d owners, want 1", n)
	}
}

func TestRemovingAMemberSignsThemOutEverywhere(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	dev, devID := e.member(t, "dev@example.com", store.RoleDeveloper)
	laptop := e.newClient()
	if code, _ := laptop.call(t, "POST", "/api/v1/session", map[string]string{"email": "dev@example.com", "password": "member password 1"}, nil); code != http.StatusOK {
		t.Fatal("second sign-in failed")
	}
	if code, _ := e.call(t, "DELETE", "/api/v1/members/"+devID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	for i, c := range []*env{dev, laptop} {
		if c.signedIn(t) {
			t.Errorf("session %d survived removal", i)
		}
	}
	if code, _ := dev.call(t, "POST", "/api/v1/session", map[string]string{"email": "dev@example.com", "password": "member password 1"}, nil); code != http.StatusUnauthorized {
		t.Errorf("sign in after removal: %d, want 401", code)
	}
	entries, _ := e.store.RecentAudit(context.Background(), 5)
	i := slices.IndexFunc(entries, func(en store.AuditEntry) bool { return en.Action == "member.removed" })
	if i < 0 || entries[i].Target != "dev@example.com" || entries[i].Actor != owner["email"] || !strings.Contains(entries[i].Detail, "2 sessions") {
		t.Errorf("audit: %+v", entries)
	}
	if code, _ := e.call(t, "DELETE", "/api/v1/members/"+devID, nil, nil); code != http.StatusNotFound {
		t.Errorf("remove twice: %d, want 404", code)
	}
}

func TestRoleChangeAppliesToTheAPIAtOnce(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	dev, devID := e.member(t, "dev@example.com", store.RoleDeveloper)
	if code, _ := dev.call(t, "GET", "/api/v1/audit", nil, nil); code != http.StatusForbidden {
		t.Fatalf("developer reads audit: %d, want 403", code)
	}
	if code, _ := e.call(t, "PATCH", "/api/v1/members/"+devID, map[string]string{"role": "admin"}, nil); code != http.StatusOK {
		t.Fatal("promote failed")
	}
	// Same session, no sign-in again.
	if code, _ := dev.call(t, "GET", "/api/v1/audit", nil, nil); code != http.StatusOK {
		t.Errorf("promoted admin reads audit: %d, want 200", code)
	}
	if _, out := dev.call(t, "GET", "/api/v1/session", nil, nil); out["role"] != "admin" {
		t.Errorf("session role: %v", out)
	}
	if code, _ := e.call(t, "PATCH", "/api/v1/members/"+devID, map[string]string{"role": "viewer"}, nil); code != http.StatusOK {
		t.Fatal("demote failed")
	}
	if code, _ := dev.call(t, "GET", "/api/v1/members", nil, nil); code != http.StatusForbidden {
		t.Errorf("demoted viewer lists members: %d, want 403", code)
	}
	entries, _ := e.store.RecentAudit(context.Background(), 1)
	if entries[0].Action != "member.role_changed" || entries[0].Detail != "admin → viewer" {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestMembersList(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	e.enrollTOTP(t)
	e.member(t, "ada@example.com", store.RoleAdmin)
	if err := e.store.CreateUser(context.Background(), &store.User{Email: "idle@example.com", Name: "Idle", PasswordHash: "x", Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	code, members := e.list(t, "/api/v1/members")
	if code != http.StatusOK || len(members) != 3 {
		t.Fatalf("members: %d %v", code, members)
	}
	byEmail := map[string]map[string]any{}
	for _, m := range members {
		byEmail[m["email"].(string)] = m
	}
	me := byEmail[owner["email"]]
	if me["you"] != true || me["role"] != "owner" || fmt.Sprint(me["secondFactor"]) != "[totp]" || me["lastActive"] == nil {
		t.Errorf("owner row: %v", me)
	}
	if idle := byEmail["idle@example.com"]; idle["lastActive"] != nil || fmt.Sprint(idle["secondFactor"]) != "[]" || idle["you"] != false {
		t.Errorf("idle row: %v", idle)
	}
	if members[0]["role"] != "owner" || members[2]["role"] != "viewer" {
		t.Errorf("not ordered by role: %v", members)
	}
}

func TestRolesMatrixIsServed(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t)
	viewer, _ := e.member(t, "v@example.com", store.RoleViewer)
	code, out := viewer.call(t, "GET", "/api/v1/roles", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("roles: %d", code)
	}
	perms := out["permissions"].([]any)
	first := perms[0].(map[string]any)
	if first["label"] == "" || first["grants"].(map[string]any)["viewer"].(map[string]any)["level"] != "yes" {
		t.Errorf("first permission: %v", first)
	}
	roles := out["roles"].([]any)
	if r := roles[2].(map[string]any); r["role"] != "developer" || r["group"] != "kwerft:role:developer" || r["clusterRole"] != "kwerft:developer" {
		t.Errorf("developer mapping: %v", r)
	}
}

func TestAuditLogFiltersAndPages(t *testing.T) {
	e := newEnv(t)
	e.completeSetup(t) // writes setup.token_accepted and setup.owner_created
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		actor := []string{"a@example.com", "b@example.com"}[i%2]
		action := []string{"member.invite", "member.role_changed", "app.create"}[i%3]
		if err := e.store.Audit(ctx, store.AuditEntry{Actor: actor, Action: action, Target: fmt.Sprint("t", i)}); err != nil {
			t.Fatal(err)
		}
	}
	type page struct {
		Entries []struct {
			ID     int64  `json:"id"`
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Target string `json:"target"`
		} `json:"entries"`
		Next *int64 `json:"next"`
	}
	get := func(query string) page {
		t.Helper()
		code, body := e.raw(t, "GET", "/api/v1/audit"+query, "")
		var p page
		if code != http.StatusOK || jsonUnmarshal(body, &p) != nil {
			t.Fatalf("audit%s: %d %s", query, code, body)
		}
		return p
	}

	// Paging walks everything once, newest first.
	var seen []int64
	cursor := ""
	for pages := 0; ; pages++ {
		p := get("?limit=25" + cursor)
		for _, en := range p.Entries {
			seen = append(seen, en.ID)
		}
		if p.Next == nil {
			break
		}
		cursor = fmt.Sprint("&before=", *p.Next)
		if pages > 10 {
			t.Fatal("paging does not end")
		}
	}
	if len(seen) != 62 || !slices.IsSortedFunc(seen, func(a, b int64) int { return int(b - a) }) {
		t.Errorf("paged %d entries (want 62), sorted newest first: %v", len(seen), seen)
	}
	if first := get("?limit=1").Entries[0]; first.Target != "t59" {
		t.Errorf("newest entry: %+v", first)
	}

	if p := get("?actor=a@example.com&limit=200"); len(p.Entries) != 30 || p.Next != nil {
		t.Errorf("actor filter: %d entries", len(p.Entries))
	}
	p := get("?action=member.&limit=200")
	if len(p.Entries) != 40 {
		t.Errorf("action prefix member.: %d entries, want 40", len(p.Entries))
	}
	for _, en := range p.Entries {
		if !strings.HasPrefix(en.Action, "member.") {
			t.Errorf("prefix filter let through %s", en.Action)
		}
	}
	if p := get("?action=member.invite&actor=b@example.com&limit=200"); len(p.Entries) != 10 {
		t.Errorf("exact action and actor: %d entries, want 10", len(p.Entries))
	}
	if p := get("?action=member_&limit=200"); len(p.Entries) != 0 {
		t.Errorf("exact match treats _ as a wildcard: %d", len(p.Entries))
	}
	for _, bad := range []string{"?limit=0", "?limit=x", "?before=-1"} {
		if code, _ := e.raw(t, "GET", "/api/v1/audit"+bad, ""); code != http.StatusBadRequest {
			t.Errorf("audit%s: %d, want 400", bad, code)
		}
	}

	code, facets := e.call(t, "GET", "/api/v1/audit/facets", nil, nil)
	if code != http.StatusOK || len(facets["actors"].([]any)) != 4 || len(facets["actions"].([]any)) != 5 {
		t.Errorf("facets: %d %v", code, facets)
	}
}

type fakeSender struct {
	mu   sync.Mutex
	sent []InviteMessage
}

func (f *fakeSender) SendInvite(_ context.Context, m InviteMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func TestInviteSenderSeam(t *testing.T) {
	sender := &fakeSender{}
	e := newEnv(t, func(c *Config) { c.InviteSender = sender; c.ConsoleDomain = "console.test" })
	e.completeSetup(t)
	code, out := e.call(t, "POST", "/api/v1/invites", map[string]string{"email": "sam@example.com", "role": "viewer"}, nil)
	if code != http.StatusCreated || out["invite"].(map[string]any)["sent"] != true {
		t.Fatalf("invite: %d %v", code, out)
	}
	if len(sender.sent) != 1 || sender.sent[0].To != "sam@example.com" || !strings.HasPrefix(sender.sent[0].URL, "https://console.test/invite/") ||
		sender.sent[0].URL != out["url"] || sender.sent[0].InvitedBy != owner["name"] {
		t.Errorf("sent: %+v", sender.sent)
	}
}

func mustUser(t *testing.T, st *store.Store, email string) *store.User {
	t.Helper()
	u, err := st.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
