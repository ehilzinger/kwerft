// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ehilzinger/kwerft/internal/backups"
)

// runEtcdSnapshot is `kwerft etcd-snapshot list|fetch`: it reads the etcd
// snapshots the node agents uploaded (<prefix>/etcd/<node>/, encrypted
// with the SSE-C key derived from the recovery key) back from the bucket,
// with the same `backups:` block and key files as install.sh --restore, so
// nobody has to craft SSE-C headers by hand. The fetched file goes to
// `k3s server --cluster-reset --cluster-reset-restore-path=<file>` (a .zip
// as k3s compressed it; k3s reads it as it is). docs/phase6.md › As built
// (B4).
func runEtcdSnapshot(args []string) int {
	return etcdSnapshotCmd(args, os.Stdout, os.Stderr)
}

// etcdSnapshotHTTP is the client for the bucket (tests swap it).
var etcdSnapshotHTTP = &http.Client{}

const etcdSnapshotUsage = `Usage:
  kwerft etcd-snapshot list  --config kwerft.yaml [--node NODE]
  kwerft etcd-snapshot fetch --config kwerft.yaml [--node NODE] [--name SNAPSHOT|latest] [--out FILE]

Reads the etcd snapshots Kwerft uploaded to the backup bucket, decrypted
with the key derived from the recovery key. kwerft.yaml is the file of
install.sh --restore:

  backups:
    endpoint: https://fsn1.your-objectstorage.com
    region: fsn1                # optional
    bucket: acme-kwerft
    prefix: ops.example.com     # the console's hostname unless Settings › Backups says otherwise
    accessKeyFile: /root/s3.access
    secretKeyFile: /root/s3.secret
    recoveryKeyFile: /root/kwerft-recovery.key

Then, on the server to restore (stop k3s first; the token is the old
server's, from /var/lib/rancher/k3s/server/token or the Secret
kwerft-system/cluster-local-join of a backup):

  k3s server --cluster-reset --cluster-reset-restore-path=FILE --token=TOKEN
`

func etcdSnapshotCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "list" && args[0] != "fetch" {
		fmt.Fprint(stderr, etcdSnapshotUsage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return 0
		}
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("etcd-snapshot "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, etcdSnapshotUsage) }
	configFile := fs.String("config", "", "the installer's config file with the backups block (required)")
	prefixFlag := fs.String("prefix", "", "the prefix in the bucket (default: backups.prefix, else domain, of the config)")
	node := fs.String("node", "", "the node whose snapshots to read (default: the only one in the bucket)")
	name := fs.String("name", "latest", "fetch: the snapshot's name, or latest")
	out := fs.String("out", "", "fetch: where to write the snapshot (default: its name, in the current directory)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *configFile == "" || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	t, cr, sseKey, err := readSnapshotConfig(*configFile, *prefixFlag)
	if err != nil {
		fmt.Fprintln(stderr, "kwerft etcd-snapshot:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c := &backups.Client{HTTP: etcdSnapshotHTTP}
	if err := etcdSnapshotRun(ctx, c, verb, t, cr, sseKey, *node, *name, *out, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "kwerft etcd-snapshot:", err)
		return 1
	}
	return 0
}

func etcdSnapshotRun(ctx context.Context, c *backups.Client, verb string, t backups.Target, cr backups.Credentials, sseKey []byte,
	node, name, out string, stdout, stderr io.Writer) error {
	nodes := []string{node}
	if node == "" {
		var err error
		if nodes, err = c.EtcdNodes(ctx, t, cr); err != nil {
			return bucketError(err)
		}
		if len(nodes) == 0 {
			return fmt.Errorf("no etcd snapshots under %s in bucket %s (check backups.prefix, and that Settings › Backups sends etcd snapshots to the bucket)", backups.EtcdFolder(t.Prefix, "<node>"), t.Bucket)
		}
	}
	cfg := backups.EtcdUploadConfig{Target: t, Credentials: cr}
	if verb == "list" {
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NODE\tSNAPSHOT\tSIZE\tTAKEN")
		for _, n := range nodes {
			snaps, err := c.BucketSnapshots(ctx, cfg, n)
			if err != nil {
				return bucketError(err)
			}
			slices.SortFunc(snaps, func(a, b backups.EtcdSnapshot) int { return b.Time.Compare(a.Time) })
			for _, s := range snaps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, s.Name, humanBytes(s.Size), s.Time.UTC().Format(time.RFC3339))
			}
		}
		return tw.Flush()
	}

	if len(nodes) > 1 {
		return fmt.Errorf("the bucket holds snapshots of several nodes (%s): choose one with --node", strings.Join(nodes, ", "))
	}
	node = nodes[0]
	if name == "" || name == "latest" {
		snaps, err := c.BucketSnapshots(ctx, cfg, node)
		if err != nil {
			return bucketError(err)
		}
		if len(snaps) == 0 {
			return fmt.Errorf("no etcd snapshots of node %s in the bucket", node)
		}
		slices.SortFunc(snaps, func(a, b backups.EtcdSnapshot) int { return b.Time.Compare(a.Time) })
		name = snaps[0].Name
	}
	if name != filepath.Base(name) || !snapshotNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a snapshot's name (kwerft etcd-snapshot list shows them)", name)
	}
	if out == "" {
		out = name
	}
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%s exists already; remove it or choose another --out", out)
	}
	tmp := out + ".kwerft-fetch"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	key := backups.EtcdFolder(t.Prefix, node) + name
	n, err := c.GetObject(ctx, t, cr, key, sseKey, f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, out)
	}
	if err != nil {
		_ = os.Remove(tmp)
		var se *backups.Error
		if errors.As(err, &se) && se.Status == http.StatusBadRequest {
			return fmt.Errorf("the storage cannot decrypt %s with the key of this recovery key file (%v): is it the key of the console that uploaded it?", key, err)
		}
		if backups.IsNotFound(err) {
			return fmt.Errorf("%s is not in the bucket (kwerft etcd-snapshot list shows what is)", key)
		}
		return bucketError(err)
	}
	fmt.Fprintf(stderr, "Fetched %s (%s, node %s) to %s.\nRestore it with: k3s server --cluster-reset --cluster-reset-restore-path=%s --token=<the old server's token>\n",
		name, humanBytes(n), node, out, out)
	return nil
}

var snapshotNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func bucketError(err error) error {
	var se *backups.Error
	if errors.As(err, &se) && (se.Status == http.StatusForbidden || se.Status == http.StatusUnauthorized) {
		return fmt.Errorf("the bucket refused the access keys: %v (check backups.accessKeyFile, secretKeyFile and region)", err)
	}
	return fmt.Errorf("cannot read the bucket: %w", err)
}

// snapshotConfig is the part of install.sh's config file this reads.
type snapshotConfig struct {
	Domain  string `json:"domain"`
	Backups *struct {
		Endpoint        string `json:"endpoint"`
		Region          string `json:"region"`
		Bucket          string `json:"bucket"`
		Prefix          string `json:"prefix"`
		AccessKeyFile   string `json:"accessKeyFile"`
		SecretKeyFile   string `json:"secretKeyFile"`
		RecoveryKeyFile string `json:"recoveryKeyFile"`
	} `json:"backups"`
}

// readSnapshotConfig reads the backups block and its key files. Errors
// name files, never their contents.
func readSnapshotConfig(file, prefix string) (backups.Target, backups.Credentials, []byte, error) {
	var t backups.Target
	var cr backups.Credentials
	raw, err := os.ReadFile(file)
	if err != nil {
		return t, cr, nil, err
	}
	var cfg snapshotConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return t, cr, nil, fmt.Errorf("%s: %w", file, err)
	}
	b := cfg.Backups
	if b == nil {
		return t, cr, nil, fmt.Errorf("%s has no backups block (endpoint, bucket, prefix, accessKeyFile, secretKeyFile, recoveryKeyFile)", file)
	}
	t = backups.Target{Endpoint: strings.TrimRight(b.Endpoint, "/"), Region: b.Region, Bucket: b.Bucket,
		Prefix: strings.Trim(cmpOr(prefix, b.Prefix, cfg.Domain), "/")}
	if !strings.HasPrefix(t.Endpoint, "https://") || strings.Contains(strings.TrimPrefix(t.Endpoint, "https://"), "/") {
		return t, cr, nil, fmt.Errorf("backups.endpoint in %s must look like https://fsn1.your-objectstorage.com, got %q", file, b.Endpoint)
	}
	if t.Bucket == "" {
		return t, cr, nil, fmt.Errorf("backups.bucket in %s is missing", file)
	}
	if t.Prefix == "" {
		return t, cr, nil, fmt.Errorf("backups.prefix in %s is needed: the folder in the bucket, by default the console's hostname (Settings › Backups shows it)", file)
	}
	read := func(field, path string) (string, error) {
		if path == "" {
			return "", fmt.Errorf("backups.%s in %s is missing", field, file)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("backups.%s in %s not readable: %w", field, file, err)
		}
		return string(raw), nil
	}
	if cr.AccessKey, err = read("accessKeyFile", b.AccessKeyFile); err != nil {
		return t, cr, nil, err
	}
	if cr.SecretKey, err = read("secretKeyFile", b.SecretKeyFile); err != nil {
		return t, cr, nil, err
	}
	cr.AccessKey, cr.SecretKey = strings.TrimSpace(cr.AccessKey), strings.TrimSpace(cr.SecretKey)
	text, err := read("recoveryKeyFile", b.RecoveryKeyFile)
	if err != nil {
		return t, cr, nil, err
	}
	key, err := backups.RecoveryKeyFromFile(text)
	if err == nil {
		var sse []byte
		if sse, err = backups.SSECustomerKey(key); err == nil {
			return t, cr, sse, nil
		}
	}
	return t, cr, nil, fmt.Errorf("backups.recoveryKeyFile (%s) does not hold a recovery key: 52 characters A-Z and 2-7, as the console showed it", b.RecoveryKeyFile)
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if strings.Trim(v, "/ ") != "" {
			return v
		}
	}
	return ""
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
