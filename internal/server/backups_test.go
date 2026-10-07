// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/backups"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Backups through the console API against the test cluster with the
// chart's RBAC, Velero's CRDs and a fake S3 bucket.

const (
	testAccessKey = "AKIATESTKWERFT"
	testSecretKey = "s3cr3t-0123456789abcdef"
)

// fakeBucket is one S3 bucket that knows one access key.
type fakeBucket struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]bool
	// sse: the SSE-C key's MD5 an object was written with; ignoreSSE
	// plays a store that keeps such objects unencrypted.
	sse       map[string]string
	bodies    map[string]string
	ignoreSSE bool
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<Error><Code>" + code + "</Code></Error>"))
	}
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential="+testAccessKey+"/") {
		fail(403, "InvalidAccessKeyId")
		return
	}
	// The fake checks the secret key by its signature: a request signed
	// with another secret does not match the one signed here.
	check := r.Clone(r.Context())
	check.Header = http.Header{}
	for k, v := range r.Header {
		if k != "Authorization" && k != "X-Amz-Date" && k != "X-Amz-Content-Sha256" && k != "Accept-Encoding" && k != "User-Agent" && k != "Content-Length" {
			check.Header[k] = v
		}
	}
	check.Host = r.Host
	check.URL.Host = r.Host
	at, _ := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	var body []byte
	if r.Body != nil {
		buf := new(strings.Builder)
		_, _ = ioCopy(buf, r)
		body = []byte(buf.String())
	}
	backups.Sign(check, body, backups.Credentials{AccessKey: testAccessKey, SecretKey: testSecretKey}, backups.RegionFor(backups.Target{Endpoint: "https://" + r.Host}), at)
	if check.Header.Get("Authorization") != auth {
		fail(403, "SignatureDoesNotMatch")
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		fail(404, "NoSuchBucket")
		return
	}
	switch {
	case r.Method == "GET" && key != "":
		switch md5 := r.Header.Get(backups.HeaderSSECKeyMD5); {
		case !f.objects[key]:
			fail(404, "NoSuchKey")
		case f.sse[key] != "" && md5 != f.sse[key]:
			fail(400, "InvalidRequest")
		default:
			_, _ = w.Write([]byte(f.bodies[key]))
		}
	case r.Method == "GET":
		var b strings.Builder
		b.WriteString("<ListBucketResult>")
		for k := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				b.WriteString("<Contents><Key>" + k + "</Key></Contents>")
			}
		}
		b.WriteString("</ListBucketResult>")
		_, _ = w.Write([]byte(b.String()))
	case r.Method == "PUT":
		f.objects[key] = true
		f.bodies[key] = string(body)
		if !f.ignoreSSE {
			f.sse[key] = r.Header.Get(backups.HeaderSSECKeyMD5)
		}
	case r.Method == "DELETE":
		delete(f.objects, key)
		w.WriteHeader(204)
	}
}

func ioCopy(dst *strings.Builder, r *http.Request) (int64, error) {
	buf := make([]byte, 4096)
	var n int64
	for {
		k, err := r.Body.Read(buf)
		dst.Write(buf[:k])
		n += int64(k)
		if err != nil {
			return n, nil
		}
	}
}

// backupsFixture is what the backup target reconciler and install.sh
// leave: the empty write-only Secrets, Velero's namespace, settings.
func backupsFixture(t *testing.T) (*fakeBucket, string, func(*Config)) {
	t.Helper()
	settingsFixture(t)
	ctx := context.Background()
	if err := cluster.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllers.VeleroNamespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	for _, name := range []string{controllers.BackupCredentialsSecret, controllers.BackupKeySecret} {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: name}}
		if err := cluster.admin.Create(ctx, sec); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), sec) })
	}
	t.Cleanup(func() {
		_ = cluster.admin.DeleteAllOf(context.Background(), &kwerftv1.BackupPlan{})
		_ = cluster.admin.DeleteAllOf(context.Background(), &kwerftv1.Restore{})
	})
	f := &fakeBucket{bucket: "acme-kwerft", objects: map[string]bool{}, sse: map[string]string{}, bodies: map[string]string{}}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL, func(c *Config) {
		withSettings()(c)
		c.System, c.SystemReader = cluster.admin, cluster.admin
		c.backupsHook = func(b *backupsAPI) { b.s3 = &backups.Client{HTTP: srv.Client()} }
	}
}

func storedSecret(t *testing.T, name string) map[string][]byte {
	t.Helper()
	var sec corev1.Secret
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: name}, &sec); err != nil {
		t.Fatal(err)
	}
	return sec.Data
}

type saveAnswer struct {
	Settings    backupTargetJSON `json:"settings"`
	RecoveryKey string           `json:"recoveryKey"`
	PlanCreated bool             `json:"planCreated"`
	Check       *checkJSON       `json:"check"`
}

func TestBackupTargetIsCheckedAndWriteOnly(t *testing.T) {
	f, endpoint, opt := backupsFixture(t)
	c := newConsole(t, opt)
	target := func(extra map[string]any) map[string]any {
		m := map[string]any{"endpoint": endpoint, "bucket": "acme-kwerft", "accessKey": testAccessKey, "secretKey": testSecretKey}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	for _, s := range []*session{c.dev, c.viewer} {
		if code := s.do(t, "GET", "/api/v1/settings/backups", nil, nil); code != http.StatusForbidden {
			t.Errorf("non-admin reads the target: %d", code)
		}
		if code := s.do(t, "PUT", "/api/v1/settings/backups", target(nil), nil); code != http.StatusForbidden {
			t.Errorf("non-admin sets the target: %d", code)
		}
	}
	var view backupTargetJSON
	if code := c.owner.do(t, "GET", "/api/v1/settings/backups", nil, &view); code != http.StatusOK || view.Configured || view.State != "NotConfigured" ||
		view.DefaultPrefix != "console.example.com" {
		t.Errorf("unconfigured: %d %+v", code, view)
	}
	for _, bad := range []struct {
		body  map[string]any
		field string
	}{
		{target(map[string]any{"endpoint": "http://plain.example.com"}), "endpoint"},
		{target(map[string]any{"endpoint": "https://x.example.com/path"}), "endpoint"},
		{target(map[string]any{"bucket": "No_Such"}), "bucket"},
		{target(map[string]any{"prefix": "../up"}), "prefix"},
		{target(map[string]any{"accessKey": "", "secretKey": ""}), "accessKey"},
		{target(map[string]any{"secretKey": ""}), "secretKey"},
		{target(map[string]any{"secretKey": "wrong"}), "secretKey"},
		{target(map[string]any{"accessKey": "AKIAOTHER"}), "accessKey"},
		{target(map[string]any{"bucket": "other-bucket"}), "bucket"},
		{target(map[string]any{"recoveryKey": "not a key"}), "recoveryKey"},
		{target(map[string]any{"etcdSnapshots": map[string]any{"enabled": true, "schedule": "every hour"}}), "etcdSnapshots.schedule"},
	} {
		var e apiError
		if code := c.owner.do(t, "PUT", "/api/v1/settings/backups", bad.body, &e); code != http.StatusBadRequest || e.Field != bad.field {
			t.Errorf("%v: %d %+v, want field %s", bad.body, code, e, bad.field)
		}
	}
	// A store that ignores SSE-C would keep every Secret of a backup
	// readable: refused.
	f.mu.Lock()
	f.ignoreSSE = true
	f.mu.Unlock()
	var e apiError
	if code := c.owner.do(t, "PUT", "/api/v1/settings/backups", target(nil), &e); code != http.StatusBadRequest || e.Field != "endpoint" ||
		!strings.Contains(e.Error, "SSE-C") {
		t.Errorf("a store without SSE-C: %d %+v", code, e)
	}
	f.mu.Lock()
	f.ignoreSSE = false
	leftover := len(f.objects)
	f.mu.Unlock()
	if leftover != 0 {
		t.Errorf("the check left %d objects", leftover)
	}
	if len(storedSecret(t, controllers.BackupCredentialsSecret)) != 0 || len(storedSecret(t, controllers.BackupKeySecret)) != 0 {
		t.Fatal("a refused target stored keys")
	}

	// The first save: checked, stored, a recovery key made and shown once,
	// the plan "cluster" created.
	res := c.owner.raw(t, "PUT", "/api/v1/settings/backups", target(nil))
	if res.code != http.StatusOK || strings.Contains(res.body, testSecretKey) || strings.Contains(res.body, testAccessKey) {
		t.Fatalf("save: %d %s", res.code, res.body)
	}
	var saved saveAnswer
	if err := json.Unmarshal([]byte(res.body), &saved); err != nil {
		t.Fatal(err)
	}
	key := saved.RecoveryKey
	if norm, err := backups.NormalizeRecoveryKey(key); err != nil || norm != key || !saved.PlanCreated || saved.Check == nil || !saved.Check.OK {
		t.Fatalf("answer %+v", saved)
	}
	if s := saved.Settings; !s.Configured || !s.CredentialsSet || !s.RecoveryKeySet || s.RecoveryKeyCreatedAt == nil || s.Prefix != "console.example.com" ||
		!s.EtcdSnapshots.Enabled {
		t.Errorf("settings %+v", s)
	}
	if got := storedSecret(t, controllers.BackupCredentialsSecret); string(got[controllers.BackupAccessKeyKey]) != testAccessKey || string(got[controllers.BackupSecretKeyKey]) != testSecretKey {
		t.Errorf("credentials stored %v", got)
	}
	if got := storedSecret(t, controllers.BackupKeySecret); string(got[controllers.BackupKeySecretKey]) != key {
		t.Errorf("key stored %q", got)
	}
	cs := settingsNow(t)
	if b := cs.Spec.Backups; b == nil || b.Endpoint != endpoint || b.Bucket != "acme-kwerft" || b.Prefix != "console.example.com" || b.EtcdSnapshots == nil {
		t.Errorf("spec %+v", cs.Spec.Backups)
	}
	var plan kwerftv1.BackupPlan
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: "cluster"}, &plan); err != nil || plan.Spec.Schedule != "0 3 * * *" ||
		plan.Spec.Scope != kwerftv1.BackupCluster || plan.Spec.Retention.Duration != 14*24*time.Hour || !*plan.Spec.Volumes {
		t.Errorf("plan %+v %v", plan.Spec, err)
	}

	// Never again: not in a read, not on a later save.
	for _, s := range []*session{c.owner} {
		if got := s.raw(t, "GET", "/api/v1/settings/backups", nil); strings.Contains(got.body, key) || strings.Contains(got.body, testSecretKey) {
			t.Error("GET returns a secret")
		}
	}
	res = c.owner.raw(t, "PUT", "/api/v1/settings/backups", map[string]any{"endpoint": endpoint, "bucket": "acme-kwerft",
		"prefix": "ops/kwerft", "etcdSnapshots": map[string]any{"enabled": false}})
	if res.code != http.StatusOK || strings.Contains(res.body, `"recoveryKey":`) || strings.Contains(res.body, key) {
		t.Fatalf("second save: %d %s", res.code, res.body)
	}
	if b := settingsNow(t).Spec.Backups; b.Prefix != "ops/kwerft" || b.EtcdSnapshots != nil {
		t.Errorf("second save spec %+v", b)
	}
	if got := storedSecret(t, controllers.BackupKeySecret); string(got[controllers.BackupKeySecretKey]) != key {
		t.Error("the recovery key changed")
	}
	e = apiError{}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/backups", target(map[string]any{"recoveryKey": backups.NewRecoveryKey()}), &e); code != http.StatusBadRequest || e.Field != "recoveryKey" {
		t.Errorf("replace the key: %d %+v", code, e)
	}

	// The check of the saved target uses the stored keys.
	var check checkJSON
	if code := c.owner.do(t, "POST", "/api/v1/settings/backups/check", map[string]any{}, &check); code != http.StatusOK || !check.OK {
		t.Errorf("check saved: %d %+v", code, check)
	}
	if code := c.dev.do(t, "POST", "/api/v1/settings/backups/check", map[string]any{}, nil); code != http.StatusForbidden {
		t.Errorf("developer checks: %d", code)
	}

	entries, err := c.store.RecentAudit(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, en := range entries {
		if strings.Contains(en.Target+en.Detail, key) || strings.Contains(en.Target+en.Detail, testSecretKey) {
			t.Errorf("audit entry carries a secret: %+v", en)
		}
		actions = append(actions, en.Action)
	}
	if !slices.Contains(actions, "settings.backups") || !slices.Contains(actions, "backup.plan_create") {
		t.Errorf("audit %v", actions)
	}

	// Kubernetes agrees: owners and admins patch both Secrets, nobody reads them.
	for _, name := range []string{controllers.BackupCredentialsSecret, controllers.BackupKeySecret} {
		for _, check := range []struct {
			role, verb string
			want       bool
		}{{"owner", "patch", true}, {"admin", "patch", true}, {"owner", "get", false}, {"admin", "get", false}, {"developer", "patch", false}, {"viewer", "patch", false}} {
			if got := canName(t, check.role, controllers.GatewayNamespace, check.verb, "", "secrets", "", name); got != check.want {
				t.Errorf("%s may %s %s: %v, want %v", check.role, check.verb, name, got, check.want)
			}
		}
	}
	// The etcd snapshot agents' configuration (the keys and the SSE-C
	// key) is the controller's and the agents' only.
	for _, role := range []string{"owner", "admin", "developer", "viewer"} {
		for _, verb := range []string{"get", "list", "patch"} {
			if canName(t, role, controllers.GatewayNamespace, verb, "", "secrets", "", backups.EtcdSecret) {
				t.Errorf("%s may %s %s", role, verb, backups.EtcdSecret)
			}
		}
	}

	// The agents' uploads, as the reconciler reports them.
	var cur kwerftv1.ConsoleSettings
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cur); err != nil {
		t.Fatal(err)
	}
	at := metav1.NewTime(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	cur.Status.Backups = &kwerftv1.BackupsStatus{State: "Ready", EtcdSnapshots: []kwerftv1.EtcdSnapshotUpload{
		{Node: "server-1", Name: "etcd-snapshot-server-1-1759665600.zip", UploadedAt: &at, CheckedAt: &at, Stored: 3},
		{Node: "server-2", CheckedAt: &at, Message: "upload of x: AccessDenied"},
	}}
	if err := cluster.admin.Status().Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	if code := c.owner.do(t, "GET", "/api/v1/settings/backups", nil, &view); code != http.StatusOK || len(view.EtcdUploads) != 2 ||
		view.EtcdUploads[0].Name != "etcd-snapshot-server-1-1759665600.zip" || view.EtcdUploads[0].UploadedAt == nil || view.EtcdUploads[0].Stored != 3 ||
		view.EtcdUploads[1].Message != "upload of x: AccessDenied" {
		t.Errorf("etcd uploads: %d %+v", code, view.EtcdUploads)
	}
}

// A console restored by hand: the prefix holds backups, and only their
// recovery key may be used with it.
func TestBackupTargetTakesAnEarlierRecoveryKey(t *testing.T) {
	f, endpoint, opt := backupsFixture(t)
	f.objects["console.example.com/velero/backups/kwerft-cluster-20261001030000/velero-backup.json"] = true
	c := newConsole(t, opt)
	body := map[string]any{"endpoint": endpoint, "bucket": "acme-kwerft", "accessKey": testAccessKey, "secretKey": testSecretKey}
	var e apiError
	if code := c.owner.do(t, "PUT", "/api/v1/settings/backups", body, &e); code != http.StatusBadRequest || e.Field != "prefix" {
		t.Fatalf("new key over old backups: %d %+v", code, e)
	}
	earlier := backups.NewRecoveryKey()
	body["recoveryKey"] = strings.ToLower(strings.ReplaceAll(earlier, "-", " "))
	res := c.owner.raw(t, "PUT", "/api/v1/settings/backups", body)
	if res.code != http.StatusOK || strings.Contains(res.body, `"recoveryKey":`) || strings.Contains(res.body, earlier) {
		t.Fatalf("save: %d %s", res.code, res.body)
	}
	if got := storedSecret(t, controllers.BackupKeySecret); string(got[controllers.BackupKeySecretKey]) != earlier {
		t.Errorf("stored %q, want the written form %q", got, earlier)
	}
}

func TestBackupPlansAPI(t *testing.T) {
	_, _, opt := backupsFixture(t)
	c := newConsole(t, opt)
	for _, bad := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"name": "Nightly", "schedule": "0 3 * * *"}, "name"},
		{map[string]any{"name": strings.Repeat("a", 41), "schedule": "0 3 * * *"}, "name"},
		{map[string]any{"name": "n", "schedule": "daily"}, "schedule"},
		{map[string]any{"name": "n", "schedule": "0 3 * * *", "scope": "Projects"}, "projects"},
		{map[string]any{"name": "n", "schedule": "0 3 * * *", "projects": []string{"shop"}}, "projects"},
		{map[string]any{"name": "n", "schedule": "0 3 * * *", "retention": "5m"}, "retention"},
		{map[string]any{"name": "n", "schedule": "0 3 * * *", "scope": "Everything"}, "scope"},
	} {
		var e apiError
		if code := c.owner.do(t, "POST", "/api/v1/backups/plans", bad.body, &e); code != http.StatusBadRequest || e.Field != bad.field {
			t.Errorf("%v: %d %+v", bad.body, code, e)
		}
	}
	if code := c.dev.do(t, "POST", "/api/v1/backups/plans", map[string]any{"name": "dev", "schedule": "0 3 * * *"}, nil); code != http.StatusForbidden {
		t.Errorf("developer creates a plan: %d", code)
	}
	var plan planJSON
	if code := c.owner.do(t, "POST", "/api/v1/backups/plans", map[string]any{"name": "shop", "scope": "Projects", "projects": []string{"shop", "api", "shop"},
		"schedule": "30  */6 * * *", "retention": "3d", "volumes": false}, &plan); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if plan.Scope != "Projects" || !slices.Equal(plan.Projects, []string{"api", "shop"}) || plan.Schedule != "30 */6 * * *" || plan.Retention != "3d" || plan.Volumes {
		t.Errorf("plan %+v", plan)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/backups/plans/shop", map[string]any{"scope": "Projects", "projects": []string{"shop"},
		"schedule": "0 4 * * *", "paused": true}, &plan); code != http.StatusOK || !plan.Paused || plan.Retention != "14d" || !plan.Volumes {
		t.Errorf("update: %d %+v", code, plan)
	}
	var list []planJSON
	if code := c.owner.do(t, "GET", "/api/v1/backups/plans", nil, &list); code != http.StatusOK || len(list) != 1 || list[0].Schedule != "0 4 * * *" {
		t.Errorf("list: %d %+v", code, list)
	}
	if code := c.viewer.do(t, "GET", "/api/v1/backups/plans", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer lists plans: %d", code)
	}

	// "Back up now" marks the plan for the reconciler.
	var run struct {
		Backup string `json:"backup"`
	}
	if code := c.owner.do(t, "POST", "/api/v1/backups/plans/shop/run", nil, &run); code != http.StatusAccepted || !strings.HasPrefix(run.Backup, "kwerft-shop-") {
		t.Errorf("run: %d %+v", code, run)
	}
	var got kwerftv1.BackupPlan
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: "shop"}, &got); err != nil ||
		got.Annotations[controllers.AnnotationRunRequested] == "" || got.Annotations[controllers.AnnotationRequestedBy] != "owner@example.com" {
		t.Errorf("run annotations %v %v", got.Annotations, err)
	}
	if code := c.owner.do(t, "POST", "/api/v1/backups/plans/ghost/run", nil, nil); code != http.StatusNotFound {
		t.Errorf("run a missing plan: %d", code)
	}
	if code := c.owner.do(t, "DELETE", "/api/v1/backups/plans/shop", nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
}

func veleroObject(t *testing.T, kind, name string, labels map[string]string, spec, status map[string]any) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(controllers.VeleroBackupGVK.GroupVersion().WithKind(kind))
	u.SetNamespace(controllers.VeleroNamespace)
	u.SetName(name)
	u.SetLabels(labels)
	u.Object["spec"] = spec
	u.Object["status"] = status
	if err := cluster.admin.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), u) })
}

func TestBackupsAndRestoresAPI(t *testing.T) {
	_, _, opt := backupsFixture(t)
	c := newConsole(t, opt)
	c.project(t, "bk-api-shop")
	planLabels := map[string]string{controllers.LabelBackupPlan: "cluster", controllers.LabelBackupScope: "Cluster"}
	veleroObject(t, "Backup", "kwerft-cluster-20261004030000", planLabels,
		map[string]any{"includedNamespaces": []any{"bk-api-shop", "kwerft-system"}, "defaultVolumesToFsBackup": true},
		map[string]any{"phase": "Completed", "startTimestamp": "2026-10-04T03:00:00Z", "completionTimestamp": "2026-10-04T03:05:00Z",
			"progress": map[string]any{"itemsBackedUp": int64(120), "totalItems": int64(120)}})
	veleroObject(t, "Backup", "kwerft-cluster-20261005030000", planLabels,
		map[string]any{"includedNamespaces": []any{"bk-api-shop", "kwerft-system"}},
		map[string]any{"phase": "InProgress", "startTimestamp": "2026-10-05T03:00:00Z"})
	veleroObject(t, "PodVolumeBackup", "pvb-1", map[string]string{"velero.io/backup-name": "kwerft-cluster-20261004030000"},
		map[string]any{"backupStorageLocation": "kwerft", "node": "n1", "pod": map[string]any{"name": "web-1", "namespace": "bk-api-shop"},
			"repoIdentifier": "", "volume": "data"},
		map[string]any{"progress": map[string]any{"totalBytes": int64(5 << 20)}})

	var list backupListJSON
	if code := c.owner.do(t, "GET", "/api/v1/backups", nil, &list); code != http.StatusOK || !list.Velero || len(list.Backups) != 2 {
		t.Fatalf("list: %d %+v", code, list)
	}
	newest, done := list.Backups[0], list.Backups[1]
	if newest.Name != "kwerft-cluster-20261005030000" || newest.Restorable || done.Plan != "cluster" || !done.Restorable ||
		!slices.Equal(done.Projects, []string{"bk-api-shop"}) || done.Items != 120 || done.Bytes != 5<<20 || !done.Volumes {
		t.Errorf("backups %+v %+v", newest, done)
	}
	if code := c.dev.do(t, "GET", "/api/v1/backups", nil, nil); code != http.StatusForbidden {
		t.Errorf("developer lists backups: %d", code)
	}

	for _, bad := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"backup": "nope", "project": "bk-api-shop"}, "backup"},
		{map[string]any{"backup": "kwerft-cluster-20261005030000", "project": "bk-api-shop"}, "backup"},
		{map[string]any{"backup": "kwerft-cluster-20261004030000", "project": "elsewhere"}, "project"},
		{map[string]any{"backup": "kwerft-cluster-20261004030000", "project": "bk-api-shop", "targetProject": "kwerft-system"}, "targetProject"},
		{map[string]any{"backup": "kwerft-cluster-20261004030000", "project": "bk-api-shop", "targetProject": "bk-api-shop-copy-with-a-name-far-too-long"}, "targetProject"},
		{map[string]any{"backup": "kwerft-cluster-20261004030000", "project": "bk-api-shop", "apps": []string{"Web"}}, "apps"},
	} {
		var e apiError
		if code := c.owner.do(t, "POST", "/api/v1/backups/restores", bad.body, &e); code != http.StatusBadRequest || e.Field != bad.field {
			t.Errorf("%v: %d %+v", bad.body, code, e)
		}
	}
	c.project(t, "bk-api-taken")
	var e apiError
	if code := c.owner.do(t, "POST", "/api/v1/backups/restores", map[string]any{"backup": "kwerft-cluster-20261004030000", "project": "bk-api-shop",
		"targetProject": "bk-api-taken"}, &e); code != http.StatusBadRequest || e.Field != "targetProject" {
		t.Errorf("existing target: %d %+v", code, e)
	}
	if code := c.dev.do(t, "POST", "/api/v1/backups/restores", map[string]any{"backup": "kwerft-cluster-20261004030000", "project": "bk-api-shop"}, nil); code != http.StatusForbidden {
		t.Errorf("developer restores: %d", code)
	}

	var rs restoreJSON
	if code := c.owner.do(t, "POST", "/api/v1/backups/restores", map[string]any{"backup": "kwerft-cluster-20261004030000", "project": "bk-api-shop",
		"targetProject": "bk-api-copy", "apps": []string{"web", "api", "web"}}, &rs); code != http.StatusCreated {
		t.Fatalf("restore: %d", code)
	}
	if !strings.HasPrefix(rs.Name, "bk-api-copy-") || !slices.Equal(rs.Apps, []string{"api", "web"}) || rs.RequestedBy != "owner@example.com" || rs.Phase != "Pending" {
		t.Errorf("restore %+v", rs)
	}
	var stored kwerftv1.Restore
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: rs.Name}, &stored); err != nil || stored.Spec.TargetProject != "bk-api-copy" {
		t.Errorf("stored %+v %v", stored.Spec, err)
	}
	var restores []restoreJSON
	if code := c.owner.do(t, "GET", "/api/v1/backups/restores", nil, &restores); code != http.StatusOK || len(restores) != 1 || restores[0].Name != rs.Name {
		t.Errorf("restores: %d %+v", code, restores)
	}
	entries, err := c.store.RecentAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(en store.AuditEntry) bool {
		return en.Action == "backup.restore" && en.Target == "bk-api-shop" && strings.Contains(en.Detail, "into bk-api-copy")
	}) {
		t.Errorf("not audited: %+v", entries)
	}
}
