// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ehilzinger/kwerft/internal/backups"
	"github.com/ehilzinger/kwerft/internal/backups/fakes3"
)

const testRecoveryKey = "abcd-efgh-ijkl-mnop-qrst-uvwx-yz23-4567-abcd-efgh-ijkl-mnop-qrst"

// The node agent's etcd part: configuration from the console's Secret,
// uploads with SSE-C, a report into its node's key, nothing logged that is
// a key.
func TestEtcdAgent(t *testing.T) {
	bucket := fakes3.New("acme", "AKIAKWERFT", "s3cr3t-key")
	srv := httptest.NewTLSServer(bucket)
	defer srv.Close()
	sseKey, _ := backups.SSECustomerKey(testRecoveryKey)
	cfg := backups.EtcdUploadConfig{Target: backups.Target{Endpoint: srv.URL, Region: "fsn1", Bucket: "acme", Prefix: "ops.example.com"},
		Credentials: backups.Credentials{AccessKey: "AKIAKWERFT", SecretKey: "s3cr3t-key"}, SSEKey: sseKey, Retention: 28}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	status := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kwerft-system", Name: backups.EtcdStatusConfigMap}, Data: map[string]string{"other": "{}"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(status).Build()

	dir := t.TempDir()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	snap := fmt.Sprintf("etcd-snapshot-server-1-%d.zip", now.Add(-time.Hour).Unix())
	data := bytes.Repeat([]byte("etcd"), 1000)
	p := filepath.Join(dir, snap)
	_ = os.WriteFile(p, data, 0o600)
	_ = os.Chtimes(p, now.Add(-time.Hour), now.Add(-time.Hour))

	var logs bytes.Buffer
	a := newEtcdAgent(c, "kwerft-system", "server-1", dir, slog.New(slog.NewJSONHandler(&logs, nil)))
	a.s3 = &backups.Client{HTTP: srv.Client()}
	a.now = func() time.Time { return now }

	// No configuration (etcd snapshots stay local): nothing happens.
	a.tick(t.Context())
	if len(bucket.Requests) != 0 || a.reported != nil {
		t.Fatalf("without a configuration: %v", bucket.Requests)
	}

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kwerft-system", Name: backups.EtcdSecret}, Data: cfg.Data()}
	if err := c.Create(t.Context(), sec); err != nil {
		t.Fatal(err)
	}
	a.tick(t.Context())
	bucket.Mu.Lock()
	o := bucket.Objects["ops.example.com/etcd/server-1/"+snap]
	bucket.Mu.Unlock()
	if o == nil || !bytes.Equal(o.Data, data) || o.KeyMD5 != fakes3.KeyMD5(sseKey) {
		t.Fatalf("not uploaded with the SSE-C key: %v", bucket.Requests)
	}
	var cm corev1.ConfigMap
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(status), &cm); err != nil {
		t.Fatal(err)
	}
	var rep backups.EtcdNodeReport
	if err := json.Unmarshal([]byte(cm.Data["server-1"]), &rep); err != nil || rep.Name != snap || rep.Stored != 1 || rep.UploadedAt == nil ||
		rep.Message != "" || cm.Data["other"] != "{}" {
		t.Fatalf("report %q: %+v %v", cm.Data, rep, err)
	}

	// Nothing changed: no pass. An hour later: a pass (a listing), no upload.
	before := len(bucket.Requests)
	now = now.Add(etcdPoll)
	a.tick(t.Context())
	if len(bucket.Requests) != before {
		t.Errorf("a pass without a reason: %v", bucket.Requests[before:])
	}
	now = now.Add(etcdResync)
	a.tick(t.Context())
	if bucket.Count("PUT ") != 1 || len(bucket.Requests) == before {
		t.Errorf("hourly pass: %v", bucket.Requests[before:])
	}

	// A wrong secret key: the report says so, and the agent tries again
	// after etcdRetry.
	sec.Data["secretKey"] = []byte("wrong-secret")
	if err := c.Update(t.Context(), sec); err != nil {
		t.Fatal(err)
	}
	a.tick(t.Context())
	_ = c.Get(t.Context(), client.ObjectKeyFromObject(status), &cm)
	_ = json.Unmarshal([]byte(cm.Data["server-1"]), &rep)
	if !strings.Contains(rep.Message, "SignatureDoesNotMatch") || !a.failed {
		t.Errorf("report after a wrong key: %+v", rep)
	}
	before = len(bucket.Requests)
	now = now.Add(etcdRetry)
	a.tick(t.Context())
	if len(bucket.Requests) == before {
		t.Error("no retry")
	}

	for _, secret := range []string{"s3cr3t-key", "wrong-secret", base64.StdEncoding.EncodeToString(sseKey), string(sseKey)} {
		if strings.Contains(logs.String(), secret) || strings.Contains(fmt.Sprint(cm.Data), secret) {
			t.Errorf("a key is in the log or the report:\n%s", logs.String())
		}
	}

	// Settings turned uploads off: the Secret goes, the agent stops.
	if err := c.Delete(t.Context(), sec); err != nil {
		t.Fatal(err)
	}
	before = len(bucket.Requests)
	now = now.Add(etcdResync)
	a.tick(t.Context())
	if len(bucket.Requests) != before {
		t.Error("uploads after the configuration went")
	}
}
