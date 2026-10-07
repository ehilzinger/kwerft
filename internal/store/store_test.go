// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kwerft.db")
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		_ = s.Close()
	}
}

func TestOnlyOneOwnerWinsARace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- s.CreateOwner(ctx, &User{Email: "owner" + string(rune('a'+i)) + "@example.com", Name: "O", PasswordHash: "x"})
		}(i)
	}
	wg.Wait()
	close(results)
	won := 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrSetupComplete):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d owners created, want exactly 1", won)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Errorf("CountUsers = %d", n)
	}
}

func TestEmailIsUniqueCaseInsensitive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.CreateOwner(ctx, &User{Email: "Mara@Example.com", Name: "Mara", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	err := s.CreateUser(ctx, &User{Email: "mara@example.com", Name: "Other", PasswordHash: "x", Role: RoleViewer})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
	u, err := s.UserByEmail(ctx, "MARA@EXAMPLE.COM")
	if err != nil || u.Role != RoleOwner || u.OrgID != DefaultOrg {
		t.Fatalf("UserByEmail = %+v, %v", u, err)
	}
}

func TestSessionsExpire(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u := &User{Email: "a@example.com", Name: "A", PasswordHash: "x"}
	if err := s.CreateOwner(ctx, u); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.CreateSession(ctx, &Session{IDHash: "h1", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, got, err := s.SessionByHash(ctx, "h1", now); err != nil || got.ID != u.ID {
		t.Fatalf("SessionByHash = %v, %v", got, err)
	}
	if _, _, err := s.SessionByHash(ctx, "h1", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session still valid: %v", err)
	}
	if n, err := s.DeleteExpiredSessions(ctx, now.Add(2*time.Hour)); err != nil || n != 1 {
		t.Errorf("DeleteExpiredSessions = %d, %v", n, err)
	}
}

func TestAuditIsNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, a := range []string{"first", "second"} {
		if err := s.Audit(ctx, AuditEntry{Actor: "setup", Action: a}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentAudit(ctx, 10)
	if err != nil || len(got) != 2 || got[0].Action != "second" {
		t.Fatalf("RecentAudit = %+v, %v", got, err)
	}
}

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUser(ctx, &User{Email: "a@example.com", Name: "A", Role: RoleOwner, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "copy.db")
	if err := s.Snapshot(ctx, copyPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(ctx, copyPath); err == nil {
		t.Fatal("overwrote an existing snapshot")
	}
	// Copies hold password hashes and sessions: owner only, under any umask.
	other := filepath.Join(dir, "other.db")
	if err := CopyDatabase(ctx, filepath.Join(dir, "kwerft.db"), other); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{copyPath, other} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if m := fi.Mode().Perm(); m != 0o600 {
			t.Errorf("%s: mode %v, want 0600", filepath.Base(p), m)
		}
	}
	c, err := Open(ctx, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if u, err := c.UserByEmail(ctx, "a@example.com"); err != nil || u.Name != "A" {
		t.Fatalf("copy: %+v %v", u, err)
	}
}
