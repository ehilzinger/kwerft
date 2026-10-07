// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ehilzinger/kwerft/internal/store"
)

func TestDatabaseKeptAcrossVersions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "kwerft.db")
	start := func(version string) string {
		t.Helper()
		path, err := backupBeforeNewVersion(ctx, dir, db, version)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := recordVersion(dir, version); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A new console has nothing to keep.
	if path := start("0.6.0-rc.3"); path != "" {
		t.Fatalf("new console copied %s", path)
	}
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, st, "owner@example.com")
	_ = st.Close()

	// A restart of the same version neither.
	if path := start("0.6.0-rc.3"); path != "" {
		t.Fatalf("restart copied %s", path)
	}

	// An upgrade keeps the database as the previous version left it.
	path := start("0.6.0-rc.4")
	if want := filepath.Join(dir, "backups", "pre-0.6.0-rc.4-from-0.6.0-rc.3.db"); path != want {
		t.Fatalf("copy = %q, want %q", path, want)
	}
	if !hasUser(t, path, "owner@example.com") {
		t.Fatal("the copy misses the owner")
	}
	if v, _ := lastVersion(dir); v != "0.6.0-rc.4" {
		t.Fatalf("recorded version = %q", v)
	}
}

// A database no version recorded (consoles before this record) is copied
// once; a copy from an earlier start of the same upgrade is kept as it is.
func TestDatabaseCopyBeforeTheRecordAndAfterAFailedStart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "kwerft.db")
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, st, "first@example.com")
	_ = st.Close()

	path, err := backupBeforeNewVersion(ctx, dir, db, "0.6.0")
	if err != nil || filepath.Base(path) != "pre-0.6.0-from-earlier.db" {
		t.Fatalf("copy = %q, %v", path, err)
	}
	// The start failed before recording the version; the database changed
	// since (a half-done migration, say). The next start keeps the first copy.
	st, err = store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, st, "second@example.com")
	_ = st.Close()
	again, err := backupBeforeNewVersion(ctx, dir, db, "0.6.0")
	if err != nil || again != path {
		t.Fatalf("second start: %q, %v; want %q", again, err, path)
	}
	if hasUser(t, path, "second@example.com") {
		t.Fatal("the copy was replaced by a later state")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left: %v", err)
	}
}

func TestCopyNamesAreFileSafe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "kwerft.db")
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if err := recordVersion(dir, "../../x y"); err != nil {
		t.Fatal(err)
	}
	path, err := backupBeforeNewVersion(ctx, dir, db, "0.6.0+build/1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(dir, "backups") || filepath.Base(path) != "pre-0.6.0+build_1-from-.._.._x_y.db" {
		t.Fatalf("copy = %s", path)
	}
}
