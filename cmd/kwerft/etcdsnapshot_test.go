// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehilzinger/kwerft/internal/backups"
	"github.com/ehilzinger/kwerft/internal/backups/fakes3"
)

// `kwerft etcd-snapshot list|fetch` with install.sh's config file: the
// snapshot comes back decrypted, ready for k3s --cluster-reset-restore-path.
func TestEtcdSnapshotFetch(t *testing.T) {
	bucket := fakes3.New("acme-kwerft", "AKIAKWERFT", "s3cr3t")
	srv := httptest.NewTLSServer(bucket)
	defer srv.Close()
	old := etcdSnapshotHTTP
	etcdSnapshotHTTP = srv.Client()
	t.Cleanup(func() { etcdSnapshotHTTP = old })

	sseKey, _ := backups.SSECustomerKey(testRecoveryKey)
	c := &backups.Client{HTTP: srv.Client()}
	tg := backups.Target{Endpoint: srv.URL, Bucket: "acme-kwerft", Prefix: "ops.example.com"}
	cr := backups.Credentials{AccessKey: "AKIAKWERFT", SecretKey: "s3cr3t"}
	older, newer := []byte("older snapshot"), bytes.Repeat([]byte("newer"), 3000)
	for name, data := range map[string][]byte{
		"etcd-snapshot-server-1-1759600000.zip": older,
		"etcd-snapshot-server-1-1759665600.zip": newer,
	} {
		if err := c.PutObject(context.Background(), tg, cr, "ops.example.com/etcd/server-1/"+name, data, sseKey); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The file Settings › Backups offers for download: the key between
	// lines of prose.
	keyFile := write("kwerft-recovery.key", "Kwerft recovery key for ops.example.com\n\n"+strings.ToUpper(testRecoveryKey)+"\n\nKeep it safe.\n")
	config := write("kwerft.yaml", "domain: ops.example.com\nbackups:\n  endpoint: "+srv.URL+"/\n  bucket: acme-kwerft\n"+
		"  accessKeyFile: "+write("s3.access", "AKIAKWERFT\n")+"\n  secretKeyFile: "+write("s3.secret", "s3cr3t\n")+"\n  recoveryKeyFile: "+keyFile+"\n")
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := etcdSnapshotCmd(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	code, out, errOut := run("list", "--config", config)
	if code != 0 || !strings.Contains(out, "server-1  etcd-snapshot-server-1-1759665600.zip") ||
		strings.Index(out, "1759665600") > strings.Index(out, "1759600000") {
		t.Fatalf("list: %d\n%s%s", code, out, errOut)
	}

	// The newest by default.
	dest := filepath.Join(dir, "restore.zip")
	code, _, errOut = run("fetch", "--config", config, "--out", dest)
	if got, _ := os.ReadFile(dest); code != 0 || !bytes.Equal(got, newer) || !strings.Contains(errOut, "--cluster-reset-restore-path="+dest) {
		t.Fatalf("fetch latest: %d %s", code, errOut)
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	// Never over an existing file.
	if code, _, errOut = run("fetch", "--config", config, "--out", dest); code != 1 || !strings.Contains(errOut, "exists already") {
		t.Errorf("over an existing file: %d %s", code, errOut)
	}
	dest2 := filepath.Join(dir, "older.zip")
	if code, _, errOut = run("fetch", "--config", config, "--node", "server-1", "--name", "etcd-snapshot-server-1-1759600000.zip", "--out", dest2); code != 0 {
		t.Fatalf("fetch by name: %s", errOut)
	}
	if got, _ := os.ReadFile(dest2); !bytes.Equal(got, older) {
		t.Error("fetch by name: wrong content")
	}

	// Another recovery key: the storage cannot decrypt; nothing is left.
	write("kwerft-recovery.key", "ZZZZ-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567-ABCD-EFGH-IJKL-MNOP-QRST\n")
	dest3 := filepath.Join(dir, "wrong.zip")
	code, _, errOut = run("fetch", "--config", config, "--out", dest3)
	if _, err := os.Stat(dest3); code != 1 || !strings.Contains(errOut, "cannot decrypt") || err == nil {
		t.Errorf("another key: %d %s", code, errOut)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*.kwerft-fetch")); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}

	// Usage errors.
	write("kwerft-recovery.key", "not a key\n")
	if code, _, errOut = run("fetch", "--config", config); code != 2 || !strings.Contains(errOut, "does not hold a recovery key") || strings.Contains(errOut, "not a key") {
		t.Errorf("bad key file: %d %s", code, errOut)
	}
	if code, _, _ = run("fetch"); code != 2 {
		t.Errorf("no config: %d", code)
	}
	if code, _, _ = run("restore"); code != 2 {
		t.Errorf("unknown verb: %d", code)
	}
	write("kwerft-recovery.key", testRecoveryKey)
	if code, _, errOut = run("fetch", "--config", config, "--name", "../../etc/passwd"); code != 1 || !strings.Contains(errOut, "not a snapshot's name") {
		t.Errorf("a path as a name: %d %s", code, errOut)
	}
}
