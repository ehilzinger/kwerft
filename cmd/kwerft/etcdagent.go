// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/backups"
)

// etcdAgent is the node agent's etcd snapshot uploader (`kwerft node-agent
// --etcd-snapshot-dir`, the DaemonSet kwerft-etcd-snapshots on etcd nodes;
// docs/phase6.md › As built (B4)). k3s keeps its snapshots locally; this
// uploads them to <prefix>/etcd/<node>/ in the backup bucket, encrypted
// with the SSE-C key derived from the recovery key, and keeps the
// retention there (backups.EtcdUploader). Its configuration is the Secret
// kwerft-etcd-backup the console writes while Settings › Backups sends etcd
// snapshots to the bucket (no Secret: nothing to do); it reports into its
// node's key of the ConfigMap kwerft-etcd-backup-status. Keys are never
// logged.
type etcdAgent struct {
	c         client.Client
	namespace string
	node      string
	dir       string
	log       *slog.Logger
	s3        *backups.Client
	now       func() time.Time
	// settle and partSize for tests (defaults: the uploader's).
	settle   time.Duration
	partSize int64

	configSum string // the configuration of the last pass
	local     string // the local snapshots of the last pass
	lastPass  time.Time
	failed    bool
	reported  *backups.EtcdNodeReport
}

const (
	etcdPoll   = time.Minute     // how often the agent looks for new snapshots
	etcdResync = time.Hour       // a pass without a new snapshot: retention, missing uploads
	etcdRetry  = 5 * time.Minute // after a failed pass
)

func newEtcdAgent(c client.Client, namespace, node, dir string, log *slog.Logger) *etcdAgent {
	return &etcdAgent{c: c, namespace: namespace, node: node, dir: dir, log: log.With("part", "etcd-snapshots"),
		// A part of 16 MiB over a slow link: minutes, not seconds.
		s3: &backups.Client{HTTP: &http.Client{Timeout: 10 * time.Minute}}, now: time.Now}
}

func (a *etcdAgent) run(ctx context.Context) {
	a.log.Info("etcd snapshot uploads", "dir", a.dir)
	t := time.NewTicker(etcdPoll)
	defer t.Stop()
	for {
		a.tick(ctx)
		select {
		case <-ctx.Done():
			// An upload cut short resumes with the next agent.
			return
		case <-t.C:
		}
	}
}

// tick runs a pass when the configuration or the local snapshots changed,
// an hour went by, or the last pass failed five minutes ago.
func (a *etcdAgent) tick(ctx context.Context) {
	sec, err := a.readSecret(ctx)
	if err != nil {
		a.log.Warn("cannot read the upload configuration", "secret", backups.EtcdSecret, "err", err)
		return
	}
	if sec == nil {
		if a.configSum != "" {
			a.log.Info("etcd snapshots no longer go to the bucket (Settings › Backups); they stay local")
		}
		a.configSum, a.local, a.failed = "", "", false
		return
	}
	sum := dataSum(sec.Data)
	cfg, cfgErr := backups.ParseEtcdUploadConfig(sec.Data)
	local, err := backups.LocalSnapshots(a.dir, a.node, a.settleOr(), a.now())
	names := make([]string, 0, len(local))
	for _, s := range local {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	localSum := strings.Join(names, "\n")
	since := a.now().Sub(a.lastPass)
	due := sum != a.configSum || localSum != a.local || since >= etcdResync || a.failed && since >= etcdRetry
	if !due {
		return
	}
	a.configSum, a.local, a.lastPass = sum, localSum, a.now()

	rep := backups.EtcdNodeReport{CheckedAt: a.now().UTC()}
	switch {
	case cfgErr != nil:
		rep.Message = "The upload configuration is incomplete: " + cfgErr.Error()
	case err != nil:
		rep.Message = "Cannot read the local snapshots: " + err.Error()
	default:
		up := &backups.EtcdUploader{Client: a.s3, Config: cfg, Node: a.node, Dir: a.dir, PartSize: a.partSize, Settle: a.settle, Now: a.now, Log: a.log}
		res, err := up.Sync(ctx)
		rep.Name, rep.Stored = res.Newest, res.Stored
		if !res.NewestAt.IsZero() {
			at := res.NewestAt.UTC()
			rep.UploadedAt = &at
		}
		if err != nil {
			rep.Message = err.Error()
		}
	}
	a.failed = rep.Message != ""
	if a.failed {
		a.log.Warn("etcd snapshot pass failed", "message", rep.Message)
	}
	if err := a.report(ctx, rep); err != nil {
		// The console creates the ConfigMap; until then, try again soon.
		a.log.Warn("cannot report the etcd snapshot uploads", "err", err)
		a.failed = true
	}
}

func (a *etcdAgent) settleOr() time.Duration {
	if a.settle > 0 {
		return a.settle
	}
	return time.Minute
}

func (a *etcdAgent) readSecret(ctx context.Context) (*corev1.Secret, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var sec corev1.Secret
	err := a.c.Get(ctx, client.ObjectKey{Namespace: a.namespace, Name: backups.EtcdSecret}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sec, nil
}

// report patches this node's key of the status ConfigMap (the console
// creates it).
func (a *etcdAgent) report(ctx context.Context, rep backups.EtcdNodeReport) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{a.node: string(raw)}})
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: a.namespace, Name: backups.EtcdStatusConfigMap}}
	if err := a.c.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return err
	}
	a.reported = &rep
	return nil
}

// dataSum tells one configuration from another without keeping it.
func dataSum(d map[string][]byte) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(d[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
