// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/store"
)

func TestBackupDatabaseKeepsTheNewestThree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	var paths []string
	for i, name := range []string{"a", "b", "c", "d"} {
		p, err := BackupDatabase(ctx, st, dir, name)
		if err != nil {
			t.Fatal(err)
		}
		// Distinct modification times, oldest first.
		at := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if paths[3] != filepath.Join(dir, "backups", "pre-d.db") {
		t.Errorf("path = %s", paths[3])
	}
	// Resumable: the copy of an upgrade is made once.
	if p, err := BackupDatabase(ctx, st, dir, "d"); err != nil || p != paths[3] {
		t.Fatalf("again: %s %v", p, err)
	}
	if _, err := BackupDatabase(ctx, st, dir, "e"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "backups"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"pre-c.db", "pre-d.db", "pre-e.db"}) {
		t.Errorf("kept %v", names)
	}
}

func TestRestorePendingDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "kwerft.db")
	backup := filepath.Join(dir, "backups", "pre-u.db")
	for p, content := range map[string]string{db: "new", db + "-wal": "wal", backup: "old"} {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u := &kwerftv1.Upgrade{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Annotations: map[string]string{AnnotationRestoreDatabase: "0.5.0"}},
		Status:     kwerftv1.UpgradeStatus{Backup: &kwerftv1.UpgradeBackup{Database: backup}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(u).Build()
	log := slog.New(slog.DiscardHandler)

	// The new version (still running before the rollback) leaves it alone.
	if err := RestorePendingDatabase(ctx, c, dir, db, "0.6.0", log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(db); string(b) != "new" {
		t.Fatal("restored by the wrong version")
	}
	if err := RestorePendingDatabase(ctx, c, dir, db, "v0.5.0", log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(db); string(b) != "old" {
		t.Errorf("database = %q, want the copy", b)
	}
	if _, err := os.Stat(db + "-wal"); !os.IsNotExist(err) {
		t.Error("the new version's WAL survived")
	}
	var got kwerftv1.Upgrade
	_ = c.Get(ctx, client.ObjectKey{Name: "u"}, &got)
	if got.Annotations[AnnotationRestoreDatabase] != RestoreDone {
		t.Errorf("annotation = %q", got.Annotations[AnnotationRestoreDatabase])
	}
	// Once only.
	_ = os.WriteFile(db, []byte("newer"), 0o600)
	if err := RestorePendingDatabase(ctx, c, dir, db, "0.5.0", log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(db); string(b) != "newer" {
		t.Error("restored twice")
	}

	// Only copies in the data directory's backups.
	evil := &kwerftv1.Upgrade{
		ObjectMeta: metav1.ObjectMeta{Name: "evil", Annotations: map[string]string{AnnotationRestoreDatabase: "0.5.0"}},
		Status:     kwerftv1.UpgradeStatus{Backup: &kwerftv1.UpgradeBackup{Database: "/etc/shadow"}},
	}
	c = fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(evil).Build()
	if err := RestorePendingDatabase(ctx, c, dir, db, "0.5.0", log); err == nil {
		t.Error("restored from outside the backups directory")
	}
}
