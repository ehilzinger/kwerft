package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The console's database across versions: the first time a version starts
// on a database another version used (an upgrade, whether install.sh ran by
// hand or the console upgraded itself), it copies the database before it
// opens it, so before its migrations and after the old version stopped (the
// Deployment's Recreate strategy). The copy goes next to the console
// upgrades' own, <dataDir>/backups/pre-<version>-from-<previous>.db, and
// shares their pruning (upgrades.KeepDatabaseBackups). <dataDir>/version
// records the version that last opened the database.
const versionFile = "version"

// unsafeFileChars are what a version may not bring into a file name.
var unsafeFileChars = regexp.MustCompile(`[^0-9A-Za-z.+_-]`)

// backupBeforeNewVersion makes that copy when the database was last opened
// by another version, or by one before this record existed. It returns the
// copy's path, "" when none was needed. A copy left by an earlier start of
// the same upgrade (the migrations failed, the pod restarted) is kept: it is
// the one from before them.
func backupBeforeNewVersion(ctx context.Context, dataDir, dbFile, current string) (string, error) {
	previous, err := lastVersion(dataDir)
	if err != nil {
		return "", err
	}
	if previous == current {
		return "", nil
	}
	if _, err := os.Stat(dbFile); errors.Is(err, fs.ErrNotExist) {
		return "", nil // a new console
	} else if err != nil {
		return "", err
	}
	dir := filepath.Join(dataDir, upgrades.BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("pre-%s-from-%s.db", unsafeFileChars.ReplaceAllString(current, "_"),
		unsafeFileChars.ReplaceAllString(cmp.Or(previous, "earlier"), "_"))
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := store.CopyDatabase(ctx, dbFile, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return path, upgrades.PruneDatabaseBackups(dir, path)
}

// lastVersion is the version that last opened the database; "" when none
// recorded it.
func lastVersion(dataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, versionFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// recordVersion notes that this version opened (and migrated) the database.
func recordVersion(dataDir, current string) error {
	path := filepath.Join(dataDir, versionFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(current+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dataDir)
}

// keepDatabaseAcrossVersions runs backupBeforeNewVersion at start. A failed
// copy does not stop the console: it is a safety net, and a console that
// will not start is the worse outcome.
func keepDatabaseAcrossVersions(ctx context.Context, log *slog.Logger, dataDir, dbFile, current string) {
	path, err := backupBeforeNewVersion(ctx, dataDir, dbFile, current)
	switch {
	case err != nil:
		log.Error("cannot copy the database before this version's first start; starting without the copy", "version", current, "err", err)
	case path != "":
		log.Info("database copied before this version's first start", "version", current, "copy", path)
	}
}
