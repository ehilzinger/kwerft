package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database written by the previous release (schema 3: users, second
// factors, invites) migrates to the Phase 4 tables without losing anything.
func TestMigrationFromPreviousSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kwerft.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:3] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO users (id, org_id, email, name, password_hash, role, created_at)
		VALUES ('u1', 'default', 'mara@example.com', 'Mara', 'hash', 'owner', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO totp (user_id, secret, confirmed, last_step, created_at) VALUES ('u1', 'v1:sealed', 1, 7, 1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("schema version %d %v, want %d", v, err, len(migrations))
	}
	u, err := s.UserByEmail(ctx, "mara@example.com")
	if err != nil || u.Role != RoleOwner {
		t.Fatalf("user after migration: %+v %v", u, err)
	}
	if tp, err := s.TOTPFor(ctx, "u1"); err != nil || tp.Secret != "v1:sealed" || !tp.Confirmed {
		t.Errorf("totp after migration: %+v %v", tp, err)
	}
	now := time.Now()
	if err := s.CreateAPIToken(ctx, &APIToken{UserID: "u1", Name: "ci", Kind: TokenKindAPI, TokenHash: "h", Hint: "kwft_x…",
		Role: RoleViewer, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Errorf("token in migrated database: %v", err)
	}
}

func tokenUser(t *testing.T, s *Store, email, role string) *User {
	t.Helper()
	u := &User{Email: email, Name: email, PasswordHash: "x", Role: role}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAPITokens(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := tokenUser(t, s, "dev@example.com", RoleDeveloper)
	other := tokenUser(t, s, "other@example.com", RoleViewer)
	now := time.Unix(1_800_000_000, 0)
	tok := &APIToken{UserID: u.ID, Name: "deploy bot", Kind: TokenKindAPI, TokenHash: "hash-1", Hint: "kwft_abcdef…",
		Role: RoleViewer, Projects: []string{"shop", "blog"}, CreatedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}
	if err := s.CreateAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, gu, err := s.APITokenByHash(ctx, "hash-1", now.Add(time.Minute))
	if err != nil || got.ID != tok.ID || gu.Email != u.Email || gu.Role != RoleDeveloper || got.Role != RoleViewer ||
		strings.Join(got.Projects, ",") != "shop,blog" || got.Name != "deploy bot" {
		t.Fatalf("by hash: %+v %+v %v", got, gu, err)
	}
	if _, _, err := s.APITokenByHash(ctx, "nope", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash: %v", err)
	}
	// Expired: refused, but with token and user for the audit log.
	if gt, gu, err := s.APITokenByHash(ctx, "hash-1", tok.ExpiresAt); !errors.Is(err, ErrTokenExpired) || gt == nil || gu == nil {
		t.Errorf("expired: %v", err)
	}
	if err := s.TokenUsed(ctx, tok.ID, now.Add(time.Hour), "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	list, err := s.APITokens(ctx, u.ID)
	if err != nil || len(list) != 1 || list[0].LastUsedIP != "203.0.113.9" || !list[0].LastUsedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("list: %+v %v", list, err)
	}
	// Unrestricted token: Projects stays nil.
	all := &APIToken{UserID: u.ID, Name: "all", Kind: TokenKindKubeconfig, TokenHash: "hash-2", Hint: "h", Role: RoleDeveloper,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateAPIToken(ctx, all); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.APITokenByHash(ctx, "hash-2", now); got.Projects != nil || got.Kind != TokenKindKubeconfig {
		t.Errorf("unrestricted: %+v", got)
	}
	// Someone else cannot revoke it; the owner can, once.
	if _, err := s.DeleteAPIToken(ctx, other.ID, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke by another user: %v", err)
	}
	if gone, err := s.DeleteAPIToken(ctx, u.ID, tok.ID); err != nil || gone.Name != "deploy bot" {
		t.Errorf("revoke: %+v %v", gone, err)
	}
	if _, _, err := s.APITokenByHash(ctx, "hash-1", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token still found: %v", err)
	}
	// Removing the user removes their tokens.
	if _, _, err := s.DeleteUser(ctx, u.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.APITokenByHash(ctx, "hash-2", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("token of a removed user: %v", err)
	}
}

func TestAPITokenLimitAndCleanup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := tokenUser(t, s, "a@example.com", RoleAdmin)
	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < MaxTokensPerUser; i++ {
		if err := s.CreateAPIToken(ctx, &APIToken{UserID: u.ID, Name: "t", Kind: TokenKindAPI, TokenHash: fmt.Sprint("h", i), Hint: "h",
			Role: RoleViewer, CreatedAt: now, ExpiresAt: now.Add(time.Duration(i+1) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	extra := &APIToken{UserID: u.ID, Name: "t", Kind: TokenKindAPI, TokenHash: "extra", Hint: "h", Role: RoleViewer, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateAPIToken(ctx, extra); !errors.Is(err, ErrTooManyTokens) {
		t.Fatalf("over the limit: %v", err)
	}
	// Expired tokens do not count, and are cleaned up after a grace period.
	later := now.Add(90 * time.Minute)
	extra.CreatedAt, extra.ExpiresAt = later, later.Add(time.Hour)
	if err := s.CreateAPIToken(ctx, extra); err != nil {
		t.Fatalf("after one expired: %v", err)
	}
	n, err := s.DeleteExpiredTokens(ctx, now.Add(3*time.Hour), time.Hour)
	if err != nil || n != 2 {
		t.Errorf("cleanup removed %d %v, want 2", n, err)
	}
}

func TestIdentitiesAndInviteBySingleSignOn(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	u := tokenUser(t, s, "mara@example.com", RoleOwner)
	const iss = "https://accounts.google.com"
	if _, err := s.UserByIdentity(ctx, iss, "sub-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no link yet: %v", err)
	}
	if err := s.LinkIdentity(ctx, Identity{Issuer: iss, Subject: "sub-1", UserID: u.ID, Email: u.Email, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserByIdentity(ctx, iss, "sub-1"); err != nil || got.ID != u.ID {
		t.Fatalf("by identity: %+v %v", got, err)
	}
	// A second account at the same provider cannot link to the same user.
	if err := s.LinkIdentity(ctx, Identity{Issuer: iss, Subject: "sub-2", UserID: u.ID, Email: u.Email, CreatedAt: now}); !errors.Is(err, ErrIdentityConflict) {
		t.Errorf("second subject: %v", err)
	}
	// Another provider may.
	if err := s.LinkIdentity(ctx, Identity{Issuer: "https://login.example.com", Subject: "sub-1", UserID: u.ID, Email: u.Email, CreatedAt: now}); err != nil {
		t.Errorf("other issuer: %v", err)
	}
	if err := s.IdentityUsed(ctx, iss, "sub-1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ids, err := s.Identities(ctx, u.ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("identities: %+v %v", ids, err)
	}
	if err := s.UnlinkIdentity(ctx, u.ID, iss); err != nil {
		t.Fatal(err)
	}
	if err := s.UnlinkIdentity(ctx, u.ID, iss); !errors.Is(err, ErrNotFound) {
		t.Errorf("unlink twice: %v", err)
	}

	// An open invite is accepted by a verified email, once.
	inv := &Invite{Email: "sam@example.com", Role: RoleDeveloper, TokenHash: "inv", InvitedByEmail: u.Email, InvitedByName: "Mara",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInviteByEmail(ctx, "nobody@example.com", &User{Name: "x"}, Identity{Issuer: iss, Subject: "s"}, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("no invite: %v", err)
	}
	if _, err := s.AcceptInviteByEmail(ctx, "sam@example.com", &User{Name: "Sam"}, Identity{Issuer: iss, Subject: "sam"}, inv.ExpiresAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired invite: %v", err)
	}
	sam := &User{Name: "Sam"}
	if _, err := s.AcceptInviteByEmail(ctx, "SAM@example.com", sam, Identity{Issuer: iss, Subject: "sam", Email: "sam@example.com"}, now); err != nil {
		t.Fatal(err)
	}
	if sam.Role != RoleDeveloper || sam.PasswordHash != "" || sam.Email != "sam@example.com" {
		t.Errorf("invited user: %+v", sam)
	}
	if got, err := s.UserByIdentity(ctx, iss, "sam"); err != nil || got.ID != sam.ID {
		t.Errorf("invited user's link: %v", err)
	}
	if _, err := s.AcceptInviteByEmail(ctx, "sam@example.com", &User{Name: "Sam"}, Identity{Issuer: iss, Subject: "sam2"}, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("invite accepted twice: %v", err)
	}

	// Auto-join: a new user and its link, together.
	kim := &User{Email: "kim@example.com", Name: "Kim", Role: RoleViewer}
	if err := s.CreateUserWithIdentity(ctx, kim, Identity{Issuer: iss, Subject: "kim", Email: kim.Email}, now); err != nil {
		t.Fatal(err)
	}
	// The same subject cannot create a second user.
	dup := &User{Email: "kim2@example.com", Name: "Kim", Role: RoleViewer}
	if err := s.CreateUserWithIdentity(ctx, dup, Identity{Issuer: iss, Subject: "kim"}, now); !errors.Is(err, ErrIdentityConflict) {
		t.Errorf("duplicate subject: %v", err)
	}
	if _, err := s.UserByEmail(ctx, "kim2@example.com"); !errors.Is(err, ErrNotFound) {
		t.Error("user of a failed auto-join was kept")
	}
}

func TestResealSecrets(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	a := tokenUser(t, s, "a@example.com", RoleOwner)
	b := tokenUser(t, s, "b@example.com", RoleViewer)
	_ = s.StartTOTP(ctx, a.ID, "old:a", now)
	_ = s.StartTOTP(ctx, b.ID, "new:b", now)
	current := func(v string) bool { return strings.HasPrefix(v, "new:") }
	if total, ok, err := s.SealedValues(ctx, current); err != nil || total != 2 || ok != 1 {
		t.Fatalf("sealed values: %d %d %v", total, ok, err)
	}
	// A failure leaves everything as it was.
	_, err := s.ResealSecrets(ctx, func(kind, owner, v string) (string, bool, error) {
		if owner == b.ID {
			return "", false, errors.New("boom")
		}
		return "new:" + owner, true, nil
	})
	if err == nil {
		t.Fatal("error swallowed")
	}
	if tp, _ := s.TOTPFor(ctx, a.ID); tp.Secret != "old:a" {
		t.Errorf("partial reseal kept: %q", tp.Secret)
	}
	n, err := s.ResealSecrets(ctx, func(kind, owner, v string) (string, bool, error) {
		if kind != SealedTOTP {
			t.Errorf("kind %q", kind)
		}
		if current(v) {
			return v, false, nil
		}
		return "new:" + owner, true, nil
	})
	if err != nil || n != 1 {
		t.Fatalf("reseal: %d %v", n, err)
	}
	if total, ok, _ := s.SealedValues(ctx, current); total != 2 || ok != 2 {
		t.Errorf("after reseal: %d of %d current", ok, total)
	}
}
