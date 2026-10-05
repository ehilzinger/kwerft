package upgrades

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Snapshotter makes a consistent copy of the console's database
// (store.Store.Snapshot, VACUUM INTO).
type Snapshotter interface {
	Snapshot(ctx context.Context, path string) error
}

// BackupDatabase writes the copy kept for an Upgrade's rollback to
// <dataDir>/backups/pre-<upgrade>.db (an existing one is kept: the backup
// step is resumable) and removes all but the newest KeepDatabaseBackups.
func BackupDatabase(ctx context.Context, s Snapshotter, dataDir, upgrade string) (string, error) {
	dir := filepath.Join(dataDir, BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, SnapshotName(upgrade)+".db")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := s.Snapshot(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, pruneBackups(dir, path)
}

// pruneBackups keeps the newest copies (by modification time); keep is
// never removed.
func pruneBackups(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type file struct {
		path string
		mod  int64
	}
	var files []file
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "pre-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	slices.SortFunc(files, func(a, b file) int {
		if a.mod != b.mod {
			if a.mod > b.mod {
				return -1
			}
			return 1
		}
		return strings.Compare(b.path, a.path)
	})
	var errs []error
	kept := 0
	for _, f := range files {
		if f.path == keep || kept < KeepDatabaseBackups-1 {
			if f.path != keep {
				kept++
			}
			continue
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RestorePendingDatabase runs when the console starts, before it opens its
// database: an Upgrade that rolled back a release with rollbackSafe: false
// asks the console of the version it rolled back to (annotation
// AnnotationRestoreDatabase) to put the copy taken before the upgrade back.
// Only that version restores, once, and only a copy in <dataDir>/backups.
func RestorePendingDatabase(ctx context.Context, c client.Client, dataDir, dbFile, version string, log *slog.Logger) error {
	var list kwerftv1.UpgradeList
	if err := c.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil // no Upgrades CRD yet: nothing to restore
		}
		return err
	}
	for i := range list.Items {
		u := &list.Items[i]
		want := u.Annotations[AnnotationRestoreDatabase]
		if want == "" || want == RestoreDone || strings.TrimPrefix(want, "v") != strings.TrimPrefix(version, "v") {
			continue
		}
		if u.Status.Backup == nil || u.Status.Backup.Database == "" {
			continue
		}
		src := u.Status.Backup.Database
		if filepath.Dir(filepath.Clean(src)) != filepath.Join(dataDir, BackupsDir) {
			return fmt.Errorf("upgrade %s: database copy %s is not in %s", u.Name, src, filepath.Join(dataDir, BackupsDir))
		}
		if err := copyOver(src, dbFile); err != nil {
			return fmt.Errorf("restore the database from %s: %w", src, err)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := os.Remove(dbFile + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		log.Warn("database restored from the copy taken before the upgrade", "upgrade", u.Name, "copy", src)
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{AnnotationRestoreDatabase: RestoreDone}}})
		if err := c.Patch(ctx, &kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: u.Name}}, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return err
		}
	}
	return nil
}

func copyOver(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".restore"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
