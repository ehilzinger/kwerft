// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehilzinger/kwerft/internal/store"
)

func addUser(t *testing.T, st *store.Store, email string) {
	t.Helper()
	if err := st.CreateUser(context.Background(), &store.User{Email: email, Name: email, Role: store.RoleAdmin, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
}

func hasUser(t *testing.T, path, email string) bool {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.UserByEmail(context.Background(), email)
	return err == nil
}

func TestSnapshotDatabaseWhileTheConsoleRuns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, err := snapshotDatabase(ctx, dir); err == nil {
		t.Fatal("snapshot without a database: want an error, not a new empty database")
	}
	if _, err := os.Stat(filepath.Join(dir, "kwerft.db")); !os.IsNotExist(err) {
		t.Fatalf("db-snapshot created a database: %v", err)
	}

	// The console holds the database open (WAL) while the hook runs.
	live, err := store.Open(ctx, filepath.Join(dir, "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	addUser(t, live, "first@example.com")

	path, size, err := snapshotDatabase(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "backup", "kwerft.db"); path != want || size == 0 {
		t.Fatalf("snapshot = %s (%d bytes), want %s", path, size, want)
	}
	if !hasUser(t, path, "first@example.com") {
		t.Fatal("the copy misses a committed user")
	}

	// The next backup replaces the copy, and leaves no temporary files.
	addUser(t, live, "second@example.com")
	if err := os.WriteFile(filepath.Join(dir, "backup", ".kwerft.db.tmp-stale"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshotDatabase(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if !hasUser(t, path, "second@example.com") {
		t.Fatal("the second snapshot did not replace the first")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "backup"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".kwerft.db.tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestApplyRestoredDatabase(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kwerft.db")

	// The backup's copy: made by the hook while "backed-up" was a user.
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, st, "backed-up@example.com")
	if _, _, err := snapshotDatabase(ctx, dir); err != nil {
		t.Fatal(err)
	}
	// Later writes reach the live files only (and their WAL).
	addUser(t, st, "after-backup@example.com")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// No marker: nothing happens.
	if done, err := applyRestoredDatabase(dir, log); err != nil || done {
		t.Fatalf("without a marker: %v %v", done, err)
	}
	if !hasUser(t, dbPath, "after-backup@example.com") {
		t.Fatal("the database changed without a marker")
	}

	// A restored volume: torn WAL and shared memory next to the live file.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(dbPath+suffix, []byte("torn by the volume backup"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(dir, "backup", "RESTORE")
	if err := os.WriteFile(marker, []byte("restored from kwerft-cluster-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done, err := applyRestoredDatabase(dir, log)
	if err != nil || !done {
		t.Fatalf("with a marker: %v %v", done, err)
	}
	for _, f := range []string{marker, dbPath + "-wal", dbPath + "-shm", dbPath + ".restoring"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still there after the swap", f)
		}
	}
	if !hasUser(t, dbPath, "backed-up@example.com") || hasUser(t, dbPath, "after-backup@example.com") {
		t.Fatal("kwerft.db is not the backup's copy")
	}
	// The copy stays: the next backup's hook replaces it.
	if _, err := os.Stat(filepath.Join(dir, "backup", "kwerft.db")); err != nil {
		t.Fatal(err)
	}
	// Once only.
	if done, err := applyRestoredDatabase(dir, log); err != nil || done {
		t.Fatalf("second start: %v %v", done, err)
	}
}

func TestApplyRestoredDatabaseRefusesAMissingOrBrokenCopy(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "kwerft.db")
	if err := os.WriteFile(live, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "backup", "RESTORE")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyRestoredDatabase(dir, log); err == nil {
		t.Fatal("marker without a copy: want an error")
	}
	if err := os.WriteFile(filepath.Join(dir, "backup", "kwerft.db"), []byte("not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := applyRestoredDatabase(dir, log)
	if err == nil || !strings.Contains(err.Error(), "not an SQLite database") {
		t.Fatalf("broken copy: %v", err)
	}
	// Nothing was touched.
	if b, _ := os.ReadFile(live); string(b) != "live" {
		t.Fatal("the live database was replaced")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the marker was removed")
	}
}
