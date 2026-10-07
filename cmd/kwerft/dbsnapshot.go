// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ehilzinger/kwerft/internal/store"
)

// The console's database in backups (docs/phase6.md): Velero runs `kwerft
// db-snapshot` in the console's container before it backs up the data
// volume (the pre-backup hook in charts/kwerft/templates/deployment.yaml),
// so the volume backup carries a consistent copy next to the live files.
// install.sh --restore writes the marker after restoring that volume, and
// the console swaps the copy in when it starts.
const (
	dbFile           = "kwerft.db"
	backupDir        = "backup"  // under the data directory
	restoreMarker    = "RESTORE" // in backupDir
	snapshotTempGlob = ".kwerft.db.tmp-*"
)

// sqliteHeader starts every SQLite database file.
var sqliteHeader = []byte("SQLite format 3\x00")

// runDBSnapshot is `kwerft db-snapshot`.
func runDBSnapshot(args []string) int {
	flags := flag.NewFlagSet("db-snapshot", flag.ExitOnError)
	dataDir := flags.String("data-dir", "/var/lib/kwerft", "the console's data directory")
	timeout := flags.Duration("timeout", 90*time.Second, "give up after this long")
	_ = flags.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	path, size, err := snapshotDatabase(ctx, *dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db-snapshot:", err)
		return 1
	}
	fmt.Printf("database snapshot %s (%d bytes)\n", path, size)
	return 0
}

// snapshotDatabase writes a consistent copy of <dataDir>/kwerft.db to
// <dataDir>/backup/kwerft.db while the console keeps using the database,
// replacing the previous copy atomically: a backup never sees a half-written
// copy. Returns the copy's path and size.
func snapshotDatabase(ctx context.Context, dataDir string) (string, int64, error) {
	live := filepath.Join(dataDir, dbFile)
	// store.Open would create an empty database: there must be one already.
	if _, err := os.Stat(live); err != nil {
		return "", 0, fmt.Errorf("no database at %s: %w", live, err)
	}
	dir := filepath.Join(dataDir, backupDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	// Leftovers of an interrupted run.
	if stale, err := filepath.Glob(filepath.Join(dir, snapshotTempGlob)); err == nil {
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}
	tmp, err := os.CreateTemp(dir, strings.TrimSuffix(snapshotTempGlob, "*"))
	if err != nil {
		return "", 0, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath) // VACUUM INTO wants to create the file itself
	defer func() { _ = os.Remove(tmpPath) }()

	// The same binary as the console that runs next to it, so opening
	// migrates nothing.
	st, err := store.Open(ctx, live)
	if err != nil {
		return "", 0, err
	}
	err = st.Snapshot(ctx, tmpPath)
	_ = st.Close()
	if err != nil {
		return "", 0, fmt.Errorf("snapshot: %w", err)
	}
	if err := syncFile(tmpPath); err != nil {
		return "", 0, err
	}
	dst := filepath.Join(dir, dbFile)
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", 0, err
	}
	if err := syncDir(dir); err != nil {
		return "", 0, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return "", 0, err
	}
	return dst, fi.Size(), nil
}

// applyRestoredDatabase runs before the console opens its database. When
// install.sh --restore left the marker <dataDir>/backup/RESTORE, the
// restored volume's live database files are whatever they were while the
// backup copied them, possibly torn; the copy the pre-backup hook made is
// the consistent one. It replaces kwerft.db with that copy, drops the live
// database's WAL and shared-memory files (they belong to the replaced file
// and would otherwise be applied to the copy), and removes the marker last,
// so an interrupted swap is simply done again on the next start.
func applyRestoredDatabase(dataDir string, log *slog.Logger) (bool, error) {
	dir := filepath.Join(dataDir, backupDir)
	marker := filepath.Join(dir, restoreMarker)
	if _, err := os.Stat(marker); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	snap := filepath.Join(dir, dbFile)
	if err := checkSQLiteFile(snap); err != nil {
		return false, fmt.Errorf("%s asks to restore the database from %s: %w (remove the marker to keep the current database)", marker, snap, err)
	}
	live := filepath.Join(dataDir, dbFile)
	tmp := live + ".restoring"
	if err := copyFile(snap, tmp); err != nil {
		return false, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(live + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	if err := os.Rename(tmp, live); err != nil {
		return false, err
	}
	if err := syncDir(dataDir); err != nil {
		return false, err
	}
	note, _ := os.ReadFile(marker)
	if err := os.Remove(marker); err != nil {
		return false, err
	}
	if err := syncDir(dir); err != nil {
		return false, err
	}
	log.Info("database restored from the backup's copy", "from", snap, "marker", strings.TrimSpace(string(note)))
	return true, nil
}

// checkSQLiteFile: path is a non-empty SQLite database file.
func checkSQLiteFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, sqliteHeader) {
		return errors.New("not an SQLite database")
	}
	return nil
}

// copyFile copies src to dst (0600) and syncs it.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename or removal in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
