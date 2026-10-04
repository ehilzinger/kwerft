package store

import (
	"context"
	"testing"
	"time"
)

func TestPasskeyHolders(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	newTestUser(t, s, "nopasskey@example.com")
	only := newTestUser(t, s, "only@example.com")
	backed := newTestUser(t, s, "backed@example.com")
	for i, u := range []*User{only, backed} {
		if err := s.AddPasskey(ctx, &Passkey{UserID: u.ID, CredentialID: []byte{byte(i)}, Name: "key", Credential: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReplaceRecoveryCodes(ctx, backed.ID, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseRecoveryCode(ctx, backed.ID, "h1", time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := s.PasskeyHolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Email != "backed@example.com" || got[1].Email != "only@example.com" {
		t.Fatalf("holders = %+v", got)
	}
	if got[0].RecoveryCodes != 1 || got[0].StrandedByMove() {
		t.Errorf("backed = %+v, want one code left and not stranded", got[0])
	}
	if got[1].Passkeys != 1 || !got[1].StrandedByMove() {
		t.Errorf("only = %+v, want stranded", got[1])
	}
}
