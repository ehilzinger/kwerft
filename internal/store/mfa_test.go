// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestUser(t *testing.T, s *Store, email string) *User {
	t.Helper()
	u := &User{Email: email, Name: "Test", PasswordHash: "x", Role: RoleDeveloper}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestTOTPLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := newTestUser(t, s, "a@example.com")
	now := time.Now()

	if err := s.StartTOTP(ctx, u.ID, "v1:first", now); err != nil {
		t.Fatal(err)
	}
	// An unconfirmed enrollment can be restarted and is not a factor yet.
	if err := s.StartTOTP(ctx, u.ID, "v1:second", now); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Factors(ctx, u.ID); f.Any() {
		t.Errorf("unconfirmed TOTP counts as a factor: %+v", f)
	}
	if ok, err := s.ConfirmTOTP(ctx, u.ID, 100); !ok || err != nil {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	if ok, _ := s.ConfirmTOTP(ctx, u.ID, 101); ok {
		t.Error("confirmed twice")
	}
	if err := s.StartTOTP(ctx, u.ID, "v1:third", now); !errors.Is(err, ErrTOTPEnabled) {
		t.Errorf("restart while enabled: %v", err)
	}
	if got, _ := s.TOTPFor(ctx, u.ID); got.Secret != "v1:second" || !got.Confirmed || got.LastStep != 100 {
		t.Errorf("totp = %+v", got)
	}
	if f, _ := s.Factors(ctx, u.ID); !f.TOTP || !f.Any() {
		t.Errorf("factors = %+v", f)
	}
	// Each step is accepted once, and never an older one.
	for _, c := range []struct {
		step int64
		want bool
	}{{100, false}, {101, true}, {101, false}, {100, false}, {103, true}} {
		if ok, _ := s.UseTOTPStep(ctx, u.ID, c.step); ok != c.want {
			t.Errorf("step %d: %v, want %v", c.step, ok, c.want)
		}
	}
	if err := s.DeleteTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TOTPFor(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := newTestUser(t, s, "a@example.com")
	if err := s.ReplaceRecoveryCodes(ctx, u.ID, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "h1", time.Now()); !ok {
		t.Error("fresh code refused")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "h1", time.Now()); ok {
		t.Error("code used twice")
	}
	other := newTestUser(t, s, "b@example.com")
	if ok, _ := s.UseRecoveryCode(ctx, other.ID, "h2", time.Now()); ok {
		t.Error("another user's code accepted")
	}
	if f, _ := s.Factors(ctx, u.ID); f.RecoveryCodes != 1 {
		t.Errorf("left = %d, want 1", f.RecoveryCodes)
	}
	_ = s.ReplaceRecoveryCodes(ctx, u.ID, []string{"h3"})
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "h2", time.Now()); ok {
		t.Error("replaced code still works")
	}
}

func TestPasskeys(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := newTestUser(t, s, "a@example.com")
	other := newTestUser(t, s, "b@example.com")
	p := &Passkey{UserID: u.ID, CredentialID: []byte{1, 2, 3}, Name: "Laptop", Credential: []byte(`{}`)}
	if err := s.AddPasskey(ctx, p); err != nil {
		t.Fatal(err)
	}
	dup := &Passkey{UserID: other.ID, CredentialID: []byte{1, 2, 3}, Name: "Copy", Credential: []byte(`{}`)}
	if err := s.AddPasskey(ctx, dup); !errors.Is(err, ErrPasskeyExists) {
		t.Errorf("duplicate credential: %v", err)
	}
	if err := s.RenamePasskey(ctx, other.ID, p.ID, "Stolen"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename by another user: %v", err)
	}
	if err := s.RenamePasskey(ctx, u.ID, p.ID, "Work laptop"); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_800_000_000, 0)
	if err := s.PasskeyUsed(ctx, p.ID, []byte(`{"n":1}`), at); err != nil {
		t.Fatal(err)
	}
	got, err := s.PasskeyByCredentialID(ctx, []byte{1, 2, 3})
	if err != nil || got.Name != "Work laptop" || string(got.Credential) != `{"n":1}` || !got.LastUsedAt.Equal(at) {
		t.Errorf("passkey = %+v, %v", got, err)
	}
	if f, _ := s.Factors(ctx, u.ID); f.Passkeys != 1 || !f.Any() {
		t.Errorf("factors = %+v", f)
	}
	if err := s.DeletePasskey(ctx, other.ID, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete by another user: %v", err)
	}
	if err := s.DeletePasskey(ctx, u.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Passkeys(ctx, u.ID); len(list) != 0 {
		t.Errorf("after delete: %v", list)
	}
}

func TestSignOutOtherSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := newTestUser(t, s, "a@example.com")
	now := time.Now()
	for _, h := range []string{"aaaa1111", "bbbb2222", "cccc3333"} {
		if err := s.CreateSession(ctx, &Session{IDHash: h, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteUserSession(ctx, u.ID, "bbbb"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserSession(ctx, u.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty prefix: %v", err)
	}
	if n, err := s.DeleteOtherSessions(ctx, u.ID, "aaaa1111"); n != 1 || err != nil {
		t.Errorf("deleted %d, %v; want 1", n, err)
	}
	if list, _ := s.UserSessions(ctx, u.ID, now); len(list) != 1 || list[0].IDHash != "aaaa1111" {
		t.Errorf("sessions = %+v", list)
	}
}
