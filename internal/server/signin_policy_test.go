package server

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/store"
)

// withTOTP gives a user a confirmed authenticator app, straight in the store.
func withTOTP(t *testing.T, st *store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.StartTOTP(ctx, userID, "v1:sealed", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ConfirmTOTP(ctx, userID, 1); !ok || err != nil {
		t.Fatalf("confirm totp: %v %v", ok, err)
	}
	if err := st.ReplaceRecoveryCodes(ctx, userID, []string{"h1-" + userID, "h2-" + userID}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnersAndAdminsResetSecondFactors(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	ownerID := mustUser(t, e.store, owner["email"]).ID
	admin, _ := e.member(t, "ada@example.com", store.RoleAdmin)
	dev, devID := e.member(t, "dev@example.com", store.RoleDeveloper)
	_, owner2ID := e.member(t, "owen@example.com", store.RoleOwner)
	withTOTP(t, e.store, devID)
	withTOTP(t, e.store, owner2ID)

	for _, tc := range []struct {
		who  *env
		id   string
		want int
		why  string
	}{
		{dev, owner2ID, http.StatusForbidden, "developers manage no one"},
		{admin, owner2ID, http.StatusForbidden, "admins do not touch owners"},
		{e, ownerID, http.StatusConflict, "one's own factors are on the Account page"},
		{admin, "nobody", http.StatusNotFound, "no such member"},
	} {
		if code, out := tc.who.call(t, "POST", "/api/v1/members/"+tc.id+"/reset-second-factor", nil, nil); code != tc.want {
			t.Errorf("%s: %d %v, want %d", tc.why, code, out, tc.want)
		}
	}
	if f, _ := e.store.Factors(context.Background(), owner2ID); !f.TOTP {
		t.Fatal("a refused reset removed the owner's factor")
	}

	code, out := admin.call(t, "POST", "/api/v1/members/"+devID+"/reset-second-factor", nil, nil)
	if code != http.StatusOK || out["signedOut"] != float64(1) {
		t.Fatalf("admin resets a developer: %d %v", code, out)
	}
	if f, _ := e.store.Factors(context.Background(), devID); f.Any() || f.RecoveryCodes != 0 {
		t.Errorf("factors after reset: %+v", f)
	}
	if dev.signedIn(t) {
		t.Error("the developer is still signed in after the reset")
	}
	if !slices.Contains(auditActions(t, e.store), "member.second_factor_reset") {
		t.Error("the reset is not audited")
	}
	// Next sign-in: the password alone, then (if required) enrolment.
	if code, out := dev.call(t, "POST", "/api/v1/session", map[string]string{"email": "dev@example.com", "password": "member password 1"}, nil); code != http.StatusOK || out["secondFactor"] != nil {
		t.Errorf("sign in after reset: %d %v", code, out)
	}
	// Owners reset owners.
	if code, _ := e.call(t, "POST", "/api/v1/members/"+owner2ID+"/reset-second-factor", nil, nil); code != http.StatusOK {
		t.Errorf("owner resets an owner: %d", code)
	}
}

func TestRequireTwoFactorSendsMembersToEnrol(t *testing.T) {
	e := newEnv(t, withMFA)
	e.completeSetup(t)
	admin, _ := e.member(t, "ada@example.com", store.RoleAdmin)
	dev, devID := e.member(t, "dev@example.com", store.RoleDeveloper)
	on := map[string]bool{"requireTwoFactor": true}

	// Owners only, and not without a factor of their own.
	if code, _ := admin.call(t, "PUT", "/api/v1/sign-in-policy", on, nil); code != http.StatusForbidden {
		t.Errorf("admin requires 2FA: %d, want 403", code)
	}
	if code, _ := e.call(t, "PUT", "/api/v1/sign-in-policy", on, nil); code != http.StatusConflict {
		t.Errorf("owner without a factor requires 2FA: %d, want 409", code)
	}
	e.enrollTOTP(t)
	if code, out := e.call(t, "PUT", "/api/v1/sign-in-policy", on, nil); code != http.StatusOK || out["requireTwoFactor"] != true {
		t.Fatalf("owner requires 2FA: %d %v", code, out)
	}
	if !slices.Contains(auditActions(t, e.store), "settings.require_two_factor") {
		t.Error("the setting is not audited")
	}

	// A member without a factor, signed in already: only enrolment answers.
	if code, out := dev.call(t, "GET", "/api/v1/roles", nil, nil); code != http.StatusForbidden || out["code"] != "enrolSecondFactor" {
		t.Errorf("developer without a factor reads roles: %d %v", code, out)
	}
	for _, path := range []string{"/api/v1/session", "/api/v1/account", "/api/v1/sign-in-policy"} {
		if code, _ := dev.call(t, "GET", path, nil, nil); code != http.StatusOK {
			t.Errorf("GET %s while enrolling: %d", path, code)
		}
	}
	if _, out := dev.call(t, "GET", "/api/v1/session", nil, nil); out["mustEnrol"] != true {
		t.Errorf("session does not say to enrol: %v", out)
	}
	// Signing in again works (never locked out) and says to enrol.
	fresh := dev.newClient()
	if code, out := fresh.call(t, "POST", "/api/v1/session", map[string]string{"email": "dev@example.com", "password": "member password 1"}, nil); code != http.StatusOK || out["mustEnrol"] != true {
		t.Errorf("sign in without a factor: %d %v", code, out)
	}
	// The owner, with a factor, is not held up.
	if code, _ := e.call(t, "GET", "/api/v1/roles", nil, nil); code != http.StatusOK {
		t.Errorf("owner with a factor: %d", code)
	}

	// Once enrolled, everything answers again; the last factor stays.
	withTOTP(t, e.store, devID)
	if code, _ := dev.call(t, "GET", "/api/v1/roles", nil, nil); code != http.StatusOK {
		t.Errorf("enrolled developer reads roles: %d", code)
	}
	if code, _ := dev.call(t, "POST", "/api/v1/account/totp/disable", map[string]string{"password": "member password 1"}, nil); code != http.StatusConflict {
		t.Errorf("removing the last factor while required: %d, want 409", code)
	}

	if code, _ := e.call(t, "PUT", "/api/v1/sign-in-policy", map[string]bool{"requireTwoFactor": false}, nil); code != http.StatusOK {
		t.Fatalf("turn it off: %d", code)
	}
	if code, _ := dev.call(t, "POST", "/api/v1/account/totp/disable", map[string]string{"password": "member password 1"}, nil); code != http.StatusNoContent {
		t.Errorf("removing the factor when not required: %d", code)
	}
}
