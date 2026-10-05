package backups

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// etcd snapshots (docs/phase6.md › As built (B4)): k3s keeps its snapshots
// locally only (its own S3 upload cannot encrypt), and Kwerft's node agent
// on every etcd node uploads them to <prefix>/etcd/<node>/<snapshot>,
// encrypted with the SSE-C key Velero's objects use (sse.go), and keeps
// the newest Retention of each kind there. `kwerft etcd-snapshot fetch`
// reads them back for k3s server --cluster-reset-restore-path.

// Names in kwerft-system: the uploader's configuration (written by the
// console, read by the node agent with get on exactly this Secret; kept out
// of every backup) and the agents' reports (one key per node, patched by
// that node's agent, read by the console into status.backups).
const (
	EtcdSecret          = "kwerft-etcd-backup"
	EtcdStatusConfigMap = "kwerft-etcd-backup-status"
)

// EtcdNodeReport is one node's entry in EtcdStatusConfigMap (JSON).
type EtcdNodeReport struct {
	// Newest snapshot in the bucket and when it got there.
	Name       string     `json:"name,omitempty"`
	UploadedAt *time.Time `json:"uploadedAt,omitempty"`
	CheckedAt  time.Time  `json:"checkedAt"`
	Stored     int        `json:"stored"`
	// Message: what went wrong in the last pass; empty when it succeeded.
	Message string `json:"message,omitempty"`
}

// EtcdUploadConfig is what the uploader needs: the console's backup target
// reconciler writes it into the Secret kwerft-system/kwerft-etcd-backup
// (Data), the node agent reads it from there (ParseEtcdUploadConfig).
type EtcdUploadConfig struct {
	Target      Target // Prefix: the backup prefix; snapshots go to <prefix>/etcd/<node>/
	Credentials Credentials
	SSEKey      []byte // 32 bytes, SSECustomerKey of the recovery key
	Retention   int    // snapshots of each kind kept per node
}

// Keys of the Secret.
const (
	etcdKeyEndpoint  = "endpoint"
	etcdKeyRegion    = "region"
	etcdKeyBucket    = "bucket"
	etcdKeyPrefix    = "prefix"
	etcdKeyAccess    = "accessKey"
	etcdKeySecret    = "secretKey"
	etcdKeySSE       = "sseCustomerKey"
	etcdKeyRetention = "retention"
)

// Data is the Secret's data.
func (c EtcdUploadConfig) Data() map[string][]byte {
	return map[string][]byte{
		etcdKeyEndpoint:  []byte(strings.TrimRight(c.Target.Endpoint, "/")),
		etcdKeyRegion:    []byte(RegionFor(c.Target)),
		etcdKeyBucket:    []byte(c.Target.Bucket),
		etcdKeyPrefix:    []byte(strings.Trim(c.Target.Prefix, "/")),
		etcdKeyAccess:    []byte(c.Credentials.AccessKey),
		etcdKeySecret:    []byte(c.Credentials.SecretKey),
		etcdKeySSE:       c.SSEKey,
		etcdKeyRetention: []byte(strconv.Itoa(c.Retention)),
	}
}

// ParseEtcdUploadConfig reads the Secret's data. Errors never quote a key.
func ParseEtcdUploadConfig(d map[string][]byte) (EtcdUploadConfig, error) {
	c := EtcdUploadConfig{
		Target: Target{Endpoint: string(d[etcdKeyEndpoint]), Region: string(d[etcdKeyRegion]),
			Bucket: string(d[etcdKeyBucket]), Prefix: strings.Trim(string(d[etcdKeyPrefix]), "/")},
		Credentials: Credentials{AccessKey: string(d[etcdKeyAccess]), SecretKey: string(d[etcdKeySecret])},
		SSEKey:      d[etcdKeySSE],
	}
	var missing []string
	for _, k := range []string{etcdKeyEndpoint, etcdKeyBucket, etcdKeyAccess, etcdKeySecret} {
		if len(d[k]) == 0 {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("the configuration lacks %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(c.Target.Endpoint, "https://") {
		return c, fmt.Errorf("the endpoint %q is not https://", c.Target.Endpoint)
	}
	if len(c.SSEKey) != SSEKeyBytes {
		return c, fmt.Errorf("the encryption key is not %d bytes", SSEKeyBytes)
	}
	r, err := strconv.Atoi(string(d[etcdKeyRetention]))
	if err != nil || r < 1 {
		r = DefaultEtcdRetention
	}
	c.Retention = r
	return c, nil
}

// DefaultEtcdRetention is how many snapshots of each kind are kept when
// the settings name no number (install.sh's default).
const DefaultEtcdRetention = 28

// EtcdFolder is where a node's snapshots go: <prefix>/etcd/<node>/.
func EtcdFolder(prefix, node string) string {
	return prefixDir(prefix) + "etcd/" + node + "/"
}

// EtcdSnapshot is a snapshot, local or in the bucket.
type EtcdSnapshot struct {
	Name string
	// Kind is the name without the node and time k3s appends:
	// "etcd-snapshot" (scheduled), "on-demand", "pre-<upgrade>" … Retention
	// counts per kind, as k3s does.
	Kind string
	// Time is the snapshot's time (from its name, else the file's or the
	// object's).
	Time time.Time
	Size int64
	// StoredAt is when the bucket got it (its LastModified); zero for a
	// local file.
	StoredAt time.Time
}

// k3s names a snapshot <name>-<node>-<unix seconds>, plus .zip when
// compressed.
var snapshotTimeRE = regexp.MustCompile(`^(.+)-([0-9]{9,11})(\.zip)?$`)

// ParseSnapshotName splits a snapshot's name into its kind and time; ok is
// false for names k3s did not make, whose kind is the name itself.
func ParseSnapshotName(name, node string) (kind string, at time.Time, ok bool) {
	m := snapshotTimeRE.FindStringSubmatch(name)
	if m == nil {
		return name, time.Time{}, false
	}
	sec, _ := strconv.ParseInt(m[2], 10, 64)
	kind = m[1]
	if node != "" {
		kind = strings.TrimSuffix(kind, "-"+node)
	}
	return kind, time.Unix(sec, 0).UTC(), true
}

func newSnapshot(name, node string, fallback time.Time, size int64) EtcdSnapshot {
	kind, at, ok := ParseSnapshotName(name, node)
	if !ok {
		at = fallback
	}
	return EtcdSnapshot{Name: name, Kind: kind, Time: at, Size: size}
}

// LocalSnapshots lists the finished snapshots in dir: regular files, not
// hidden, unchanged for settle (k3s writes a compressed snapshot's .zip in
// place; a name with a .zip beside it is being compressed).
func LocalSnapshots(dir, node string, settle time.Duration, now time.Time) ([]EtcdSnapshot, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	var out []EtcdSnapshot
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".part") || strings.HasSuffix(n, ".tmp") || !e.Type().IsRegular() || names[n+".zip"] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // gone meanwhile (k3s pruned it)
		}
		if now.Sub(info.ModTime()) < settle {
			continue
		}
		out = append(out, newSnapshot(n, node, info.ModTime().UTC(), info.Size()))
	}
	return out, nil
}

// BucketSnapshots lists a node's snapshots in the bucket.
func (c *Client) BucketSnapshots(ctx context.Context, cfg EtcdUploadConfig, node string) ([]EtcdSnapshot, error) {
	folder := EtcdFolder(cfg.Target.Prefix, node)
	objs, err := c.ListObjects(ctx, cfg.Target, cfg.Credentials, folder)
	if err != nil {
		return nil, err
	}
	out := make([]EtcdSnapshot, 0, len(objs))
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, folder)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		snap := newSnapshot(name, node, o.LastModified.UTC(), o.Size)
		snap.StoredAt = o.LastModified.UTC()
		out = append(out, snap)
	}
	return out, nil
}

// EtcdNodes lists the node folders under <prefix>/etcd/ (from the objects'
// keys).
func (c *Client) EtcdNodes(ctx context.Context, t Target, cr Credentials) ([]string, error) {
	folder := prefixDir(t.Prefix) + "etcd/"
	objs, err := c.ListObjects(ctx, t, cr, folder)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, folder)
		if node, _, ok := strings.Cut(rest, "/"); ok && node != "" && !slices.Contains(nodes, node) {
			nodes = append(nodes, node)
		}
	}
	slices.Sort(nodes)
	return nodes, nil
}

// Keep picks the snapshots to keep: the newest retention of each kind.
// The snapshots an Upgrade takes first (pre-<upgrade>, one kind each)
// count as one kind, or the bucket would keep every one of them.
func Keep(snaps []EtcdSnapshot, retention int) map[string]bool {
	byKind := map[string][]EtcdSnapshot{}
	for _, s := range snaps {
		kind := s.Kind
		if strings.HasPrefix(kind, "pre-") {
			kind = "pre-"
		}
		byKind[kind] = append(byKind[kind], s)
	}
	keep := map[string]bool{}
	for _, list := range byKind {
		slices.SortFunc(list, newestFirst)
		for i, s := range list {
			if i < retention {
				keep[s.Name] = true
			}
		}
	}
	return keep
}

func newestFirst(a, b EtcdSnapshot) int {
	return cmp.Or(b.Time.Compare(a.Time), strings.Compare(b.Name, a.Name))
}

// EtcdUploader uploads one node's local snapshots.
type EtcdUploader struct {
	Client *Client
	Config EtcdUploadConfig
	Node   string
	Dir    string // k3s's snapshot directory (/var/lib/rancher/k3s/server/db/snapshots)
	// PartSize of multipart uploads (default 16 MiB, at least 5 MiB): one
	// part is in memory at a time.
	PartSize int64
	// Settle: a file younger than this may still be written (default 1m).
	Settle time.Duration
	Now    func() time.Time
	Log    *slog.Logger
}

// EtcdSyncResult is what one pass did and found.
type EtcdSyncResult struct {
	// Newest is the newest snapshot in the bucket after the pass ("" with
	// none); NewestAt when it got there (the object's time, or the pass's
	// for one uploaded now).
	Newest   string
	NewestAt time.Time
	Uploaded []string
	Deleted  []string
	// Stored is how many of the node's snapshots the bucket holds.
	Stored int
}

const (
	defaultPartSize = 16 << 20
	defaultSettle   = time.Minute
)

// Sync makes the bucket hold the node's snapshots to keep: it uploads those
// missing there (newest first; an upload cut short resumes), deletes those
// beyond the retention (by kind; also ones the node no longer has), and
// aborts unfinished uploads of snapshots it does not want. Errors of single
// snapshots do not stop the others; the first is returned.
func (u *EtcdUploader) Sync(ctx context.Context) (EtcdSyncResult, error) {
	var res EtcdSyncResult
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	log := u.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg := u.Config
	local, err := LocalSnapshots(u.Dir, u.Node, cmp.Or(u.Settle, defaultSettle), now())
	if err != nil {
		return res, fmt.Errorf("cannot read the local snapshots: %w", err)
	}
	remote, err := u.Client.BucketSnapshots(ctx, cfg, u.Node)
	if err != nil {
		return res, fmt.Errorf("cannot list the bucket: %w", err)
	}
	inBucket := make(map[string]EtcdSnapshot, len(remote))
	for _, s := range remote {
		inBucket[s.Name] = s
	}
	all := slices.Clone(remote)
	for _, s := range local {
		if _, ok := inBucket[s.Name]; !ok {
			all = append(all, s)
		}
	}
	keep := Keep(all, max(cfg.Retention, 1))
	var upload []EtcdSnapshot
	for _, s := range local {
		if _, ok := inBucket[s.Name]; !ok && keep[s.Name] {
			upload = append(upload, s)
		}
	}
	slices.SortFunc(upload, newestFirst)
	folder := EtcdFolder(cfg.Target.Prefix, u.Node)
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	// Unfinished uploads of snapshots that are no longer wanted.
	wanted := map[string]bool{}
	for _, s := range upload {
		wanted[folder+s.Name] = true
	}
	if pending, err := u.Client.ListMultipartUploads(ctx, cfg.Target, cfg.Credentials, folder); err != nil {
		log.Warn("cannot list unfinished uploads", "err", err)
	} else {
		for _, p := range pending {
			if !wanted[p.Key] {
				if err := u.Client.AbortMultipartUpload(ctx, cfg.Target, cfg.Credentials, p.Key, p.UploadID); err != nil {
					log.Warn("cannot abort an unfinished upload", "key", p.Key, "err", err)
				}
			}
		}
	}

	for _, s := range upload {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		started := now()
		if err := u.upload(ctx, folder+s.Name, filepath.Join(u.Dir, s.Name)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // k3s pruned it meanwhile
			}
			log.Warn("etcd snapshot upload failed", "snapshot", s.Name, "err", err)
			fail(fmt.Errorf("upload of %s: %w", s.Name, err))
			continue
		}
		done := now()
		log.Info("etcd snapshot uploaded", "snapshot", s.Name, "bytes", s.Size, "took", done.Sub(started).Round(time.Second).String())
		res.Uploaded = append(res.Uploaded, s.Name)
		s.StoredAt = done
		inBucket[s.Name] = s
	}

	for _, s := range remote {
		if keep[s.Name] {
			continue
		}
		if err := u.Client.DeleteObject(ctx, cfg.Target, cfg.Credentials, folder+s.Name); err != nil {
			log.Warn("cannot delete an old etcd snapshot from the bucket", "snapshot", s.Name, "err", err)
			fail(fmt.Errorf("delete of %s: %w", s.Name, err))
			continue
		}
		log.Info("old etcd snapshot deleted from the bucket", "snapshot", s.Name)
		res.Deleted = append(res.Deleted, s.Name)
		delete(inBucket, s.Name)
	}

	res.Stored = len(inBucket)
	var newest *EtcdSnapshot
	for _, s := range inBucket {
		if newest == nil || newestFirst(s, *newest) < 0 {
			newest = &s
		}
	}
	if newest != nil {
		res.Newest, res.NewestAt = newest.Name, newest.StoredAt
	}
	return res, firstErr
}

func (u *EtcdUploader) upload(ctx context.Context, key, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return u.Client.UploadFile(ctx, u.Config.Target, u.Config.Credentials, key, f, info.Size(), u.Config.SSEKey, cmp.Or(u.PartSize, defaultPartSize))
}
