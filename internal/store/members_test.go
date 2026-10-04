package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeleteUserRemovesEverythingOfTheirs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	owner := &User{Email: "o@example.com", Name: "O", PasswordHash: "x", Role: RoleOwner}
	dev := &User{Email: "d@example.com", Name: "D", PasswordHash: "x", Role: RoleDeveloper}
	for _, u := range []*User{owner, dev} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"s1", "s2"} {
		if err := s.CreateSession(ctx, &Session{IDHash: h, UserID: dev.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.StartTOTP(ctx, dev.ID, "sealed", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPasskey(ctx, &Passkey{UserID: dev.ID, CredentialID: []byte{1}, Name: "k", Credential: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceRecoveryCodes(ctx, dev.ID, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}

	refuse := errors.New("no")
	if _, _, err := s.DeleteUser(ctx, dev.ID, func(*User) error { return refuse }); err != refuse {
		t.Fatalf("check not honoured: %v", err)
	}
	gone, n, err := s.DeleteUser(ctx, dev.ID, nil)
	if err != nil || n != 2 || gone.Email != dev.Email {
		t.Fatalf("delete: %v %d %v", gone, n, err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions) + (SELECT COUNT(*) FROM totp) + (SELECT COUNT(*) FROM passkeys) + (SELECT COUNT(*) FROM recovery_codes)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d rows of the removed user remain", left)
	}
	if _, _, err := s.DeleteUser(ctx, owner.ID, nil); !errors.Is(err, ErrLastOwner) {
		t.Errorf("delete last owner: %v", err)
	}
	if _, err := s.SetRole(ctx, owner.ID, RoleAdmin, nil); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demote last owner: %v", err)
	}
	if _, err := s.SetRole(ctx, owner.ID, RoleOwner, nil); err != nil {
		t.Errorf("no-op role change of the last owner: %v", err)
	}
}

func TestInviteLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	inv := &Invite{Email: "sam@example.com", Role: RoleViewer, TokenHash: "h1", InvitedByEmail: "o@example.com", InvitedByName: "O",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInvite(ctx, &Invite{Email: "SAM@example.com", Role: RoleAdmin, TokenHash: "h2", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, ErrInvitePending) {
		t.Errorf("second open invite: %v", err)
	}
	// Once expired, a new invite replaces the old one.
	later := now.Add(2 * time.Hour)
	fresh := &Invite{Email: "sam@example.com", Role: RoleAdmin, TokenHash: "h3", CreatedAt: later, ExpiresAt: later.Add(time.Hour)}
	if err := s.CreateInvite(ctx, fresh); err != nil {
		t.Fatalf("replace expired invite: %v", err)
	}
	if old, _ := s.InviteByID(ctx, inv.ID); old.State(later) != InviteRevoked {
		t.Errorf("replaced invite is %s", old.State(later))
	}
	if open, _ := s.OpenInvites(ctx); len(open) != 1 || open[0].ID != fresh.ID {
		t.Errorf("open invites: %+v", open)
	}

	u := &User{Name: "Sam", PasswordHash: "x", Role: RoleOwner} // role comes from the invite
	if _, err := s.AcceptInvite(ctx, "h1", u, later); !errors.Is(err, ErrInviteRevoked) {
		t.Errorf("accept replaced invite: %v", err)
	}
	got, err := s.AcceptInvite(ctx, "h3", u, later)
	if err != nil || got.ID != fresh.ID || u.Role != RoleAdmin || u.Email != "sam@example.com" {
		t.Fatalf("accept: %+v %+v %v", got, u, err)
	}
	if _, err := s.AcceptInvite(ctx, "h3", &User{Name: "Again", PasswordHash: "x"}, later); !errors.Is(err, ErrInviteUsed) {
		t.Errorf("accept twice: %v", err)
	}
	if _, err := s.AcceptInvite(ctx, "nope", &User{}, later); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token: %v", err)
	}
	if err := s.CreateInvite(ctx, &Invite{Email: "sam@example.com", Role: RoleViewer, TokenHash: "h4", CreatedAt: later, ExpiresAt: later.Add(time.Hour)}); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("invite a member: %v", err)
	}
	if _, err := s.RevokeInvite(ctx, fresh.ID, later, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke accepted invite: %v", err)
	}
}

func TestQueryAuditPrefixIsNotALikePattern(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, a := range []string{"member.invite", "member.removed", "memberXinvite", "members.list", "member"} {
		if err := s.Audit(ctx, AuditEntry{Actor: "a", Action: a}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.QueryAudit(ctx, AuditQuery{Action: "member."})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || page.Entries[0].Action != "member.removed" || page.Next != 0 {
		t.Errorf("prefix member.: %+v", page)
	}
}
