package backups_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/backups"
	"github.com/ehilzinger/kwerft/internal/backups/fakes3"
)

const testRecoveryKey = "abcd-efgh-ijkl-mnop-qrst-uvwx-yz23-4567-abcd-efgh-ijkl-mnop-qrst"

type etcdFixture struct {
	bucket *fakes3.Bucket
	up     *backups.EtcdUploader
	dir    string
	now    time.Time
	sseKey []byte
}

func newEtcdFixture(t *testing.T) *etcdFixture {
	t.Helper()
	b := fakes3.New("acme", "AK", "SK")
	srv := httptest.NewTLSServer(b)
	t.Cleanup(srv.Close)
	key, err := backups.SSECustomerKey(testRecoveryKey)
	if err != nil {
		t.Fatal(err)
	}
	f := &etcdFixture{bucket: b, dir: t.TempDir(), now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), sseKey: key}
	f.up = &backups.EtcdUploader{
		Client: &backups.Client{HTTP: srv.Client()},
		Config: backups.EtcdUploadConfig{Target: backups.Target{Endpoint: srv.URL, Region: "fsn1", Bucket: "acme", Prefix: "ops.example.com"},
			Credentials: backups.Credentials{AccessKey: "AK", SecretKey: "SK"}, SSEKey: key, Retention: 3},
		Node: "server-1", Dir: f.dir, PartSize: backups.MinPartSize,
		Now: func() time.Time { return f.now },
	}
	return f
}

// snapshot writes a local snapshot as k3s names it, taken at, finished a
// while before now.
func (f *etcdFixture) snapshot(t *testing.T, kind string, at time.Time, size int) (string, []byte) {
	t.Helper()
	name := fmt.Sprintf("%s-server-1-%d.zip", kind, at.Unix())
	data := make([]byte, size)
	_, _ = rand.Read(data)
	p := filepath.Join(f.dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return name, data
}

func (f *etcdFixture) stored(t *testing.T) []string {
	t.Helper()
	f.bucket.Mu.Lock()
	defer f.bucket.Mu.Unlock()
	var out []string
	for k := range f.bucket.Objects {
		out = append(out, strings.TrimPrefix(k, "ops.example.com/etcd/server-1/"))
	}
	slices.Sort(out)
	return out
}

func TestEtcdUploaderEncryptsEverySnapshot(t *testing.T) {
	f := newEtcdFixture(t)
	ctx := context.Background()
	small, smallData := f.snapshot(t, "etcd-snapshot", f.now.Add(-6*time.Hour), 1000)
	// Bigger than a part: a multipart upload of 5 MiB parts.
	big, bigData := f.snapshot(t, "etcd-snapshot", f.now.Add(-time.Hour), 12<<20)
	// Still being written (compressed in place): not yet.
	fresh := filepath.Join(f.dir, fmt.Sprintf("etcd-snapshot-server-1-%d", f.now.Unix()))
	_ = os.WriteFile(fresh, []byte("x"), 0o600)
	_ = os.WriteFile(fresh+".zip", []byte("partial"), 0o600)
	for _, p := range []string{fresh, fresh + ".zip"} {
		_ = os.Chtimes(p, f.now, f.now)
	}
	_ = os.WriteFile(filepath.Join(f.dir, ".hidden"), []byte("x"), 0o600)

	res, err := f.up.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.stored(t); !slices.Equal(got, []string{small, big}) && !slices.Equal(got, []string{big, small}) {
		t.Fatalf("bucket holds %v", got)
	}
	if res.Newest != big || !res.NewestAt.Equal(f.now) || res.Stored != 2 || !slices.Equal(res.Uploaded, []string{big, small}) {
		t.Errorf("result %+v (newest first)", res)
	}
	f.bucket.Mu.Lock()
	for name, want := range map[string][]byte{small: smallData, big: bigData} {
		o := f.bucket.Objects["ops.example.com/etcd/server-1/"+name]
		if !bytes.Equal(o.Data, want) {
			t.Errorf("%s: content differs (%d bytes, want %d)", name, len(o.Data), len(want))
		}
		// Encrypted by the storage with the key derived from the recovery key.
		if o.KeyMD5 != fakes3.KeyMD5(f.sseKey) {
			t.Errorf("%s: stored with SSE-C key MD5 %q", name, o.KeyMD5)
		}
	}
	f.bucket.Mu.Unlock()
	if n := f.bucket.Count("PUT /acme/ops.example.com/etcd/server-1/" + big + "?partNumber,uploadId"); n != 3 {
		t.Errorf("%d parts uploaded, want 3", n)
	}
	if n := f.bucket.Count("PUT /acme/ops.example.com/etcd/server-1/" + small + "?"); n != 1 {
		t.Errorf("the small snapshot took %d PUTs", n)
	}

	// Without the key the snapshot cannot be read; with it, it can.
	var buf bytes.Buffer
	cfg := f.up.Config
	if _, err := f.up.Client.GetObject(ctx, cfg.Target, cfg.Credentials, "ops.example.com/etcd/server-1/"+big, nil, &buf); err == nil {
		t.Error("read without the key")
	}
	buf.Reset()
	if _, err := f.up.Client.GetObject(ctx, cfg.Target, cfg.Credentials, "ops.example.com/etcd/server-1/"+big, f.sseKey, &buf); err != nil || !bytes.Equal(buf.Bytes(), bigData) {
		t.Errorf("read with the key: %v", err)
	}

	// Nothing new: nothing uploaded again. The fresh one, once settled, is.
	before := f.bucket.Count("PUT ")
	f.now = f.now.Add(2 * time.Minute)
	_ = os.Remove(fresh)
	res, err = f.up.Sync(ctx)
	if err != nil || len(res.Uploaded) != 1 || f.bucket.Count("PUT ") != before+1 || res.Stored != 3 {
		t.Errorf("second pass: %+v %v", res, err)
	}
}

func TestEtcdUploaderRetention(t *testing.T) {
	f := newEtcdFixture(t)
	ctx := context.Background()
	var scheduled []string
	for i := range 5 {
		n, _ := f.snapshot(t, "etcd-snapshot", f.now.Add(-time.Duration(5-i)*6*time.Hour), 100)
		scheduled = append(scheduled, n)
	}
	upgrade, _ := f.snapshot(t, "pre-upgrade-0-3-0", f.now.Add(-48*time.Hour), 100)
	// One the node no longer has, older than the three newest: deleted.
	f.bucket.Mu.Lock()
	gone := fmt.Sprintf("etcd-snapshot-server-1-%d.zip", f.now.Add(-60*time.Hour).Unix())
	f.bucket.Objects["ops.example.com/etcd/server-1/"+gone] = &fakes3.Object{Data: []byte("old"), Modified: f.now.Add(-60 * time.Hour)}
	// Another node's: never touched.
	f.bucket.Objects["ops.example.com/etcd/server-2/etcd-snapshot-server-2-1.zip"] = &fakes3.Object{Data: []byte("other")}
	f.bucket.Mu.Unlock()

	res, err := f.up.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The three newest scheduled ones and the upgrade's (a kind of its own);
	// the two older local ones are never uploaded just to be deleted.
	want := append([]string{}, scheduled[2:]...)
	want = append(want, upgrade, "../server-2/etcd-snapshot-server-2-1.zip")
	got := f.stored(t)
	for i, g := range got {
		if strings.HasPrefix(g, "ops.example.com/etcd/server-2/") {
			got[i] = "../server-2/" + strings.TrimPrefix(g, "ops.example.com/etcd/server-2/")
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("bucket holds\n %v\nwant\n %v", got, want)
	}
	if !slices.Equal(res.Deleted, []string{gone}) || res.Stored != 4 || res.Newest != scheduled[4] {
		t.Errorf("result %+v", res)
	}

	// Retention lowered to 1 in Settings: the bucket follows.
	f.up.Config.Retention = 1
	res, err = f.up.Sync(ctx)
	if err != nil || res.Stored != 2 || len(res.Deleted) != 2 {
		t.Errorf("retention 1: %+v %v", res, err)
	}
}

// An agent that stops half way through a big snapshot (a failed part, a
// restart) resumes the multipart upload: parts already stored are not sent
// again.
func TestEtcdUploaderResumes(t *testing.T) {
	f := newEtcdFixture(t)
	ctx := context.Background()
	name, data := f.snapshot(t, "etcd-snapshot", f.now.Add(-time.Hour), 17<<20) // 4 parts
	failing := true
	f.bucket.FailPart = func(_ string, n int) bool { return failing && n == 3 }

	if _, err := f.up.Sync(ctx); err == nil || !strings.Contains(err.Error(), "part 3 of 4") {
		t.Fatalf("first pass: %v", err)
	}
	if got := f.bucket.Uploads(); len(got) != 1 || len(f.stored(t)) != 0 {
		t.Fatalf("after the failure: uploads %v, objects %v", got, f.stored(t))
	}

	// A new agent (nothing in memory) picks the upload up.
	failing = false
	up := *f.up
	res, err := up.Sync(ctx)
	if err != nil || !slices.Equal(res.Uploaded, []string{name}) {
		t.Fatalf("second pass: %+v %v", res, err)
	}
	prefix := "PUT /acme/ops.example.com/etcd/server-1/" + name + "?partNumber,uploadId"
	// 1, 2, 3 (failed) the first time; 3 and 4 the second.
	if n := f.bucket.Count(prefix); n != 5 {
		t.Errorf("%d part uploads, want 5", n)
	}
	if n := f.bucket.Count("POST /acme/ops.example.com/etcd/server-1/" + name + "?uploads"); n != 1 {
		t.Errorf("%d multipart uploads started, want 1", n)
	}
	f.bucket.Mu.Lock()
	o := f.bucket.Objects["ops.example.com/etcd/server-1/"+name]
	f.bucket.Mu.Unlock()
	if o == nil || !bytes.Equal(o.Data, data) || o.KeyMD5 != fakes3.KeyMD5(f.sseKey) {
		t.Fatal("the resumed upload is not the snapshot, encrypted")
	}
	if got := f.bucket.Uploads(); len(got) != 0 {
		t.Errorf("unfinished uploads left: %v", got)
	}
}

// An unfinished upload of a snapshot k3s has pruned meanwhile is aborted;
// one with another key (the recovery key changed) starts afresh.
func TestEtcdUploaderAbortsStaleUploads(t *testing.T) {
	f := newEtcdFixture(t)
	ctx := context.Background()
	cfg := f.up.Config
	other := bytes.Repeat([]byte{7}, 32)
	if _, err := f.up.Client.CreateMultipartUpload(ctx, cfg.Target, cfg.Credentials, "ops.example.com/etcd/server-1/etcd-snapshot-server-1-1000000000.zip", f.sseKey); err != nil {
		t.Fatal(err)
	}
	name, data := f.snapshot(t, "etcd-snapshot", f.now.Add(-time.Hour), 11<<20)
	if _, err := f.up.Client.CreateMultipartUpload(ctx, cfg.Target, cfg.Credentials, "ops.example.com/etcd/server-1/"+name, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.up.Sync(ctx); err == nil {
		t.Fatal("a part with another key went through")
	}
	if got := f.bucket.Uploads(); len(got) != 0 {
		t.Fatalf("uploads left: %v", got)
	}
	if _, err := f.up.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.bucket.Mu.Lock()
	defer f.bucket.Mu.Unlock()
	if o := f.bucket.Objects["ops.example.com/etcd/server-1/"+name]; o == nil || !bytes.Equal(o.Data, data) {
		t.Fatal("not uploaded")
	}
}

func TestEtcdUploadConfig(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	c := backups.EtcdUploadConfig{Target: backups.Target{Endpoint: "https://fsn1.your-objectstorage.com/", Bucket: "acme", Prefix: "/ops.example.com/"},
		Credentials: backups.Credentials{AccessKey: "AK", SecretKey: "SK"}, SSEKey: key, Retention: 7}
	d := c.Data()
	if string(d["region"]) != "fsn1" || string(d["prefix"]) != "ops.example.com" || string(d["endpoint"]) != "https://fsn1.your-objectstorage.com" {
		t.Errorf("data %q", d)
	}
	got, err := backups.ParseEtcdUploadConfig(d)
	if err != nil || got.Retention != 7 || got.Target.Region != "fsn1" || !bytes.Equal(got.SSEKey, key) || got.Credentials.SecretKey != "SK" {
		t.Errorf("parsed %+v %v", got, err)
	}
	d["retention"] = []byte("x")
	if got, _ := backups.ParseEtcdUploadConfig(d); got.Retention != backups.DefaultEtcdRetention {
		t.Errorf("retention %d", got.Retention)
	}
	d["sseCustomerKey"] = []byte("short")
	if _, err := backups.ParseEtcdUploadConfig(d); err == nil || strings.Contains(err.Error(), "short") {
		t.Errorf("short key: %v", err)
	}
	delete(d, "secretKey")
	if _, err := backups.ParseEtcdUploadConfig(d); err == nil || !strings.Contains(err.Error(), "secretKey") {
		t.Errorf("missing key: %v", err)
	}
}

func TestParseSnapshotName(t *testing.T) {
	for _, c := range []struct{ name, kind string }{
		{"etcd-snapshot-server-1-1759665600.zip", "etcd-snapshot"},
		{"etcd-snapshot-server-1-1759665600", "etcd-snapshot"},
		{"on-demand-server-1-1759665600", "on-demand"},
		{"pre-upgrade-0-3-0-server-1-1759665600.zip", "pre-upgrade-0-3-0"},
	} {
		kind, at, ok := backups.ParseSnapshotName(c.name, "server-1")
		if !ok || kind != c.kind || at.Unix() != 1759665600 {
			t.Errorf("%s: %q %v %v", c.name, kind, at, ok)
		}
	}
	if kind, _, ok := backups.ParseSnapshotName("my-copy.db", "server-1"); ok || kind != "my-copy.db" {
		t.Errorf("foreign name: %q %v", kind, ok)
	}
}

func TestListObjectsPages(t *testing.T) {
	b := fakes3.New("acme", "AK", "SK")
	b.PageSize = 2
	for i := range 5 {
		b.Objects[fmt.Sprintf("p/etcd/n%d/s", i)] = &fakes3.Object{Data: []byte("x")}
	}
	srv := httptest.NewTLSServer(b)
	defer srv.Close()
	c := &backups.Client{HTTP: srv.Client()}
	tg := backups.Target{Endpoint: srv.URL, Bucket: "acme", Prefix: "p"}
	objs, err := c.ListObjects(context.Background(), tg, backups.Credentials{AccessKey: "AK", SecretKey: "SK"}, "p/")
	if err != nil || len(objs) != 5 {
		t.Fatalf("%d objects, %v", len(objs), err)
	}
	nodes, err := c.EtcdNodes(context.Background(), tg, backups.Credentials{AccessKey: "AK", SecretKey: "SK"})
	if err != nil || !slices.Equal(nodes, []string{"n0", "n1", "n2", "n3", "n4"}) {
		t.Errorf("nodes %v %v", nodes, err)
	}
	// A wrong secret: the fake checks every signature.
	if _, err := c.ListObjects(context.Background(), tg, backups.Credentials{AccessKey: "AK", SecretKey: "wrong"}, "p/"); err == nil {
		t.Error("a wrong signature was accepted")
	}
}

func TestKeepCountsUpgradeSnapshotsAsOneKind(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var snaps []backups.EtcdSnapshot
	for i, kind := range []string{"pre-upgrade-0-3-0", "pre-upgrade-0-3-1", "pre-upgrade-0-4-0", "on-demand", "etcd-snapshot"} {
		snaps = append(snaps, backups.EtcdSnapshot{Name: fmt.Sprintf("%s-n-%d", kind, i), Kind: kind, Time: at.Add(time.Duration(i) * time.Hour)})
	}
	keep := backups.Keep(snaps, 2)
	if keep["pre-upgrade-0-3-0-n-0"] || !keep["pre-upgrade-0-3-1-n-1"] || !keep["pre-upgrade-0-4-0-n-2"] || !keep["on-demand-n-3"] || !keep["etcd-snapshot-n-4"] {
		t.Errorf("keep %v", keep)
	}
}
