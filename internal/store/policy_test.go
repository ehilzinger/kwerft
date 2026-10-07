// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRequireTwoFactorSetting(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if on, err := s.RequireTwoFactor(ctx); err != nil || on {
		t.Fatalf("default = %v, %v; want off", on, err)
	}
	for _, want := range []bool{true, true, false} {
		if err := s.SetRequireTwoFactor(ctx, want); err != nil {
			t.Fatal(err)
		}
		if on, err := s.RequireTwoFactor(ctx); err != nil || on != want {
			t.Errorf("after set %v: %v, %v", want, on, err)
		}
	}
}

func TestResetSecondFactors(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := newTestUser(t, s, "reset@example.com")
	other := newTestUser(t, s, "other@example.com")
	now := time.Now()
	for _, id := range []string{u.ID, other.ID} {
		if err := s.StartTOTP(ctx, id, "v1:secret", now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ConfirmTOTP(ctx, id, 1); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceRecoveryCodes(ctx, id, []string{"h1-" + id, "h2-" + id}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddPasskey(ctx, &Passkey{UserID: id, CredentialID: []byte("cred-" + id), Name: "key", Credential: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(ctx, &Session{IDHash: "sess-" + id, UserID: id, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}

	refused := errors.New("no")
	if _, _, _, err := s.ResetSecondFactors(ctx, u.ID, func(*User) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("check not applied: %v", err)
	}
	if f, _ := s.Factors(ctx, u.ID); !f.TOTP {
		t.Fatal("a refused reset changed something")
	}

	got, before, sessions, err := s.ResetSecondFactors(ctx, u.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != u.Email || !before.TOTP || before.Passkeys != 1 || before.RecoveryCodes != 2 || sessions != 1 {
		t.Errorf("reset = %s %+v %d", got.Email, before, sessions)
	}
	if f, _ := s.Factors(ctx, u.ID); f.TOTP || f.Passkeys != 0 || f.RecoveryCodes != 0 {
		t.Errorf("factors after reset = %+v", f)
	}
	if list, _ := s.UserSessions(ctx, u.ID, now); len(list) != 0 {
		t.Errorf("sessions after reset: %d", len(list))
	}
	// Nobody else is touched.
	if f, _ := s.Factors(ctx, other.ID); !f.TOTP || f.Passkeys != 1 || f.RecoveryCodes != 2 {
		t.Errorf("other user's factors = %+v", f)
	}
	if _, _, _, err := s.ResetSecondFactors(ctx, "nobody", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}
