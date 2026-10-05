package controllers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/backups"
)

// Backups against the API server with Velero's CRDs (testdata/crds, the
// release install.sh pins). No Velero runs: the tests play it, setting the
// status of its objects.

func getVelero(t *testing.T, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	t.Helper()
	u := newVelero(gvk)
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: VeleroNamespace, Name: name}, u)
	return u, err
}

// setVeleroStatus plays Velero: its CRDs have no status subresource.
func setVeleroStatus(t *testing.T, gvk schema.GroupVersionKind, name string, status map[string]any) {
	t.Helper()
	eventually(t, func() error {
		u, err := getVelero(t, gvk, name)
		if err != nil {
			return err
		}
		u.Object["status"] = status
		return k8s.Update(context.Background(), u)
	})
}

func secretData(t *testing.T, namespace, name string) (map[string][]byte, *corev1.Secret, error) {
	t.Helper()
	var sec corev1.Secret
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &sec)
	return sec.Data, &sec, err
}

func TestBackupTarget(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	// k3s's own S3 configuration of an earlier version: k3s uploaded
	// without encryption. Kwerft's goes; anyone else's stays.
	legacy := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: etcdS3Namespace, Name: EtcdS3Secret,
		Labels: map[string]string{LabelManagedBy: ManagedByKwerft}}, Type: etcdS3Type,
		Data: map[string][]byte{"etcd-s3-bucket": []byte("acme-kwerft")}}
	if err := k8s.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	// An etcd node whose agent reports below.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "etcd-1"}}
	if err := k8s.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	s := useSettings(t, kwerftv1.ConsoleSettingsSpec{Backups: &kwerftv1.BackupSettings{
		Endpoint: "https://fsn1.your-objectstorage.com", Bucket: "acme-kwerft", Prefix: "ops.example.com",
		EtcdSnapshots: &kwerftv1.EtcdSnapshotSettings{}}})
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, node)
		_ = k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: etcdS3Namespace, Name: EtcdS3Secret}})
		_ = k8s.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: GatewayNamespace, Name: backups.EtcdStatusConfigMap}})
		for _, name := range []string{BackupCredentialsSecret, BackupKeySecret} {
			_, sec, err := secretData(t, GatewayNamespace, name)
			if err == nil {
				sec.Data = nil
				_ = k8s.Update(ctx, sec)
			}
		}
		bsl := newVelero(VeleroBSLGVK)
		bsl.SetNamespace(VeleroNamespace)
		bsl.SetName(BackupLocation)
		_ = k8s.Delete(ctx, bsl)
		_ = k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: VeleroNamespace, Name: RepoPasswordSecret}})
		_ = k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: VeleroNamespace, Name: BackupEncryptionSecret}})
	})

	// The reconciler makes the write-only Secrets; the key's stays out of
	// backups. Without keys the target is not configured.
	eventually(t, func() error {
		_, sec, err := secretData(t, GatewayNamespace, BackupKeySecret)
		if err != nil {
			return err
		}
		if sec.Labels[labelVeleroExclude] != "true" {
			return errors.New("the recovery key would be backed up")
		}
		_, _, err = secretData(t, GatewayNamespace, BackupCredentialsSecret)
		return err
	})
	waitForBackups(t, func(st *kwerftv1.BackupsStatus) error {
		if st.State != "NotConfigured" || !strings.Contains(st.Message, "access keys") {
			return fmt.Errorf("state %s: %s", st.State, st.Message)
		}
		return nil
	})

	// Settings (here: the test) write the keys and mark them.
	key := backups.NewRecoveryKey()
	for name, data := range map[string]map[string][]byte{
		BackupCredentialsSecret: {BackupAccessKeyKey: []byte("AKIAKWERFT"), BackupSecretKeyKey: []byte("s3cr3t")},
		BackupKeySecret:         {BackupKeySecretKey: []byte(strings.ToLower(key))},
	} {
		_, sec, err := secretData(t, GatewayNamespace, name)
		if err != nil {
			t.Fatal(err)
		}
		sec.Data = data
		if err := k8s.Update(ctx, sec); err != nil {
			t.Fatal(err)
		}
	}
	keyAt := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	annotateBackupSettings(t, map[string]string{AnnotationBackupCredentialsUpdated: keyAt.Format(time.RFC3339Nano),
		AnnotationBackupKeyCreated: keyAt.Format(time.RFC3339Nano)})

	eventually(t, func() error {
		data, _, err := secretData(t, VeleroNamespace, BackupLocationSecret)
		if err != nil {
			return err
		}
		if got := string(data[BackupLocationSecretKey]); got != "[default]\naws_access_key_id=AKIAKWERFT\naws_secret_access_key=s3cr3t\n" {
			return fmt.Errorf("credentials file %q", got)
		}
		data, _, err = secretData(t, VeleroNamespace, RepoPasswordSecret)
		if err != nil {
			return err
		}
		if got := string(data[RepoPasswordSecretKey]); got != strings.ReplaceAll(key, "-", "") {
			return fmt.Errorf("repository password %q, want the 52 characters of %q", got, key)
		}
		// The SSE-C key, derived from the recovery key: 32 bytes, as the
		// AWS plugin reads them.
		data, _, err = secretData(t, VeleroNamespace, BackupEncryptionSecret)
		if err != nil {
			return err
		}
		if want, _ := backups.SSECustomerKey(key); len(data[BackupEncryptionSecretKey]) != 32 || !bytes.Equal(data[BackupEncryptionSecretKey], want) {
			return fmt.Errorf("SSE-C key %x, want %x", data[BackupEncryptionSecretKey], want)
		}
		return nil
	})
	bsl, err := getVelero(t, VeleroBSLGVK, BackupLocation)
	if err != nil {
		t.Fatal(err)
	}
	if nestedString(bsl, "spec", "objectStorage", "prefix") != "ops.example.com/velero" || nestedString(bsl, "spec", "objectStorage", "bucket") != "acme-kwerft" ||
		nestedString(bsl, "spec", "config", "region") != "fsn1" || nestedString(bsl, "spec", "config", "s3Url") != "https://fsn1.your-objectstorage.com" ||
		nestedString(bsl, "spec", "credential", "name") != BackupLocationSecret || nestedString(bsl, "spec", "provider") != "aws" ||
		nestedString(bsl, "spec", "config", "customerKeyEncryptionSecret") != "kwerft-bsl-encryption/sse-c-key" {
		t.Errorf("location %v", bsl.Object["spec"])
	}
	if def, _, _ := unstructured.NestedBool(bsl.Object, "spec", "default"); !def {
		t.Error("the location is not the default")
	}
	// The node agents' upload configuration: the same bucket and SSE-C
	// key, never backed up.
	data, sec, err := secretData(t, GatewayNamespace, backups.EtcdSecret)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := backups.ParseEtcdUploadConfig(data)
	sseKey, _ := backups.SSECustomerKey(key)
	if err != nil || cfg.Target.Endpoint != "https://fsn1.your-objectstorage.com" || cfg.Target.Region != "fsn1" || cfg.Target.Bucket != "acme-kwerft" ||
		cfg.Target.Prefix != "ops.example.com" || cfg.Credentials.AccessKey != "AKIAKWERFT" || cfg.Credentials.SecretKey != "s3cr3t" ||
		!bytes.Equal(cfg.SSEKey, sseKey) || cfg.Retention != 28 {
		t.Errorf("etcd upload configuration %+v %v", cfg, err)
	}
	if sec.Labels[labelVeleroExclude] != "true" || sec.Labels[LabelManagedBy] != ManagedByKwerft {
		t.Errorf("etcd upload configuration labels %v", sec.Labels)
	}
	// k3s's unencrypted upload is gone.
	eventually(t, func() error {
		if _, _, err := secretData(t, etcdS3Namespace, EtcdS3Secret); !apierrors.IsNotFound(err) {
			return fmt.Errorf("kube-system/%s: %v", EtcdS3Secret, err)
		}
		return nil
	})
	// The agents' reports: nodes that are gone are left out.
	var reports corev1.ConfigMap
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: backups.EtcdStatusConfigMap}, &reports)
	})
	uploaded := time.Date(2026, 10, 5, 12, 0, 3, 0, time.UTC)
	rep, _ := json.Marshal(backups.EtcdNodeReport{Name: "etcd-snapshot-etcd-1-1759665600.zip", UploadedAt: &uploaded, CheckedAt: uploaded, Stored: 4})
	gone, _ := json.Marshal(backups.EtcdNodeReport{Name: "x", CheckedAt: uploaded})
	reports.Data = map[string]string{"etcd-1": string(rep), "removed-node": string(gone)}
	if err := k8s.Update(ctx, &reports); err != nil {
		t.Fatal(err)
	}
	waitForBackups(t, func(st *kwerftv1.BackupsStatus) error {
		if len(st.EtcdSnapshots) != 1 {
			return fmt.Errorf("etcd snapshots %+v", st.EtcdSnapshots)
		}
		e := st.EtcdSnapshots[0]
		if e.Node != "etcd-1" || e.Name != "etcd-snapshot-etcd-1-1759665600.zip" || e.UploadedAt == nil || !e.UploadedAt.Time.Equal(uploaded) ||
			e.Stored != 4 || e.Message != "" {
			return fmt.Errorf("etcd snapshot %+v", e)
		}
		return nil
	})
	waitForBackups(t, func(st *kwerftv1.BackupsStatus) error {
		if st.State != "Pending" || st.RecoveryKeyCreatedAt == nil || !st.RecoveryKeyCreatedAt.Time.Equal(keyAt) {
			return fmt.Errorf("state %s, key %v", st.State, st.RecoveryKeyCreatedAt)
		}
		return nil
	})

	// Velero's verdict on the bucket.
	setVeleroStatus(t, VeleroBSLGVK, BackupLocation, map[string]any{"phase": "Available", "lastValidationTime": "2026-10-05T12:01:00Z"})
	waitForBackups(t, func(st *kwerftv1.BackupsStatus) error {
		if st.State != "Ready" || st.CheckedAt == nil {
			return fmt.Errorf("state %s: %s", st.State, st.Message)
		}
		return nil
	})
	setVeleroStatus(t, VeleroBSLGVK, BackupLocation, map[string]any{"phase": "Unavailable", "message": "AccessDenied: bucket acme-kwerft"})
	waitForBackups(t, func(st *kwerftv1.BackupsStatus) error {
		if st.State != "Error" || !strings.Contains(st.Message, "AccessDenied") {
			return fmt.Errorf("state %s: %s", st.State, st.Message)
		}
		return nil
	})

	// Retention from the settings.
	eventually(t, func() error {
		var cur kwerftv1.ConsoleSettings
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), &cur); err != nil {
			return err
		}
		cur.Spec.Backups.EtcdSnapshots = &kwerftv1.EtcdSnapshotSettings{Retention: 7}
		return k8s.Update(ctx, &cur)
	})
	eventually(t, func() error {
		data, _, err := secretData(t, GatewayNamespace, backups.EtcdSecret)
		if err != nil {
			return err
		}
		if cfg, err := backups.ParseEtcdUploadConfig(data); err != nil || cfg.Retention != 7 {
			return fmt.Errorf("retention %d %v", cfg.Retention, err)
		}
		return nil
	})

	// Someone else's Secret of k3s's old name is not touched.
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: etcdS3Namespace, Name: EtcdS3Secret}, Type: etcdS3Type}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	// Without etcd snapshot settings the snapshots stay local: the agents'
	// configuration goes, and with it their reports.
	eventually(t, func() error {
		var cur kwerftv1.ConsoleSettings
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(s), &cur); err != nil {
			return err
		}
		cur.Spec.Backups.EtcdSnapshots = nil
		return k8s.Update(ctx, &cur)
	})
	eventually(t, func() error {
		if _, _, err := secretData(t, GatewayNamespace, backups.EtcdSecret); !apierrors.IsNotFound(err) {
			return fmt.Errorf("upload configuration still there: %v", err)
		}
		return nil
	})
	waitForBackups(t, func(st *kwerftv1.BackupsStatus) error {
		if len(st.EtcdSnapshots) != 0 {
			return fmt.Errorf("etcd snapshots %+v", st.EtcdSnapshots)
		}
		return nil
	})
	if _, _, err := secretData(t, etcdS3Namespace, EtcdS3Secret); err != nil {
		t.Errorf("someone else's kube-system/%s: %v", EtcdS3Secret, err)
	}
}

// After install.sh --restore the recovery key is only Velero's repository
// password (kwerft-backup-key is never backed up): the reconciler takes it
// back from there.
func TestBackupTargetAdoptsTheRestoredKey(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	key := backups.NewRecoveryKey()
	repo := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: VeleroNamespace, Name: RepoPasswordSecret},
		Data: map[string][]byte{RepoPasswordSecretKey: []byte(strings.ReplaceAll(key, "-", ""))}}
	if err := k8s.Create(ctx, repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, repo) })
	useSettings(t, kwerftv1.ConsoleSettingsSpec{Backups: &kwerftv1.BackupSettings{
		Endpoint: "https://hel1.your-objectstorage.com", Bucket: "acme-kwerft", Prefix: "ops.example.com"}})
	t.Cleanup(func() {
		for _, name := range []string{BackupCredentialsSecret, BackupKeySecret} {
			if _, sec, err := secretData(t, GatewayNamespace, name); err == nil {
				sec.Data = nil
				_ = k8s.Update(ctx, sec)
			}
		}
		bsl := newVelero(VeleroBSLGVK)
		bsl.SetNamespace(VeleroNamespace)
		bsl.SetName(BackupLocation)
		_ = k8s.Delete(ctx, bsl)
	})
	eventually(t, func() error {
		_, sec, err := secretData(t, GatewayNamespace, BackupCredentialsSecret)
		if err != nil {
			return err
		}
		sec.Data = map[string][]byte{BackupAccessKeyKey: []byte("AK"), BackupSecretKeyKey: []byte("SK")}
		return k8s.Update(ctx, sec)
	})
	annotateBackupSettings(t, map[string]string{AnnotationBackupCredentialsUpdated: time.Now().UTC().Format(time.RFC3339Nano)})
	eventually(t, func() error {
		data, _, err := secretData(t, GatewayNamespace, BackupKeySecret)
		if err != nil {
			return err
		}
		if string(data[BackupKeySecretKey]) != key {
			return fmt.Errorf("key %q, want %q", data[BackupKeySecretKey], key)
		}
		return nil
	})
	waitForBackups(t, func(st *kwerftv1.BackupsStatus) error {
		if st.State != "Pending" {
			return fmt.Errorf("state %s: %s", st.State, st.Message)
		}
		return nil
	})
	if data, _, _ := secretData(t, VeleroNamespace, RepoPasswordSecret); string(data[RepoPasswordSecretKey]) != strings.ReplaceAll(key, "-", "") {
		t.Errorf("repository password changed: %q", data[RepoPasswordSecretKey])
	}
}

func annotateBackupSettings(t *testing.T, ann map[string]string) {
	t.Helper()
	eventually(t, func() error {
		var cur kwerftv1.ConsoleSettings
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cur); err != nil {
			return err
		}
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		for k, v := range ann {
			cur.Annotations[k] = v
		}
		return k8s.Update(context.Background(), &cur)
	})
}

func waitForBackups(t *testing.T, check func(*kwerftv1.BackupsStatus) error) {
	t.Helper()
	eventually(t, func() error {
		var s kwerftv1.ConsoleSettings
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return err
		}
		if s.Status.Backups == nil {
			return errors.New("no status.backups yet")
		}
		return check(s.Status.Backups)
	})
}

func backupProject(t *testing.T, name string, spec kwerftv1.ProjectSpec) {
	t.Helper()
	p := &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), p) })
}

func plan(t *testing.T, name string, spec kwerftv1.BackupPlanSpec) *kwerftv1.BackupPlan {
	t.Helper()
	p := &kwerftv1.BackupPlan{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), p) })
	return p
}

func scheduleOf(t *testing.T, plan string, check func(spec map[string]any, template map[string]any) error) *unstructured.Unstructured {
	t.Helper()
	var u *unstructured.Unstructured
	eventually(t, func() error {
		var err error
		if u, err = getVelero(t, VeleroScheduleGVK, BackupScheduleName(plan)); err != nil {
			return err
		}
		spec, _ := u.Object["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		return check(spec, template)
	})
	return u
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, s := range l {
		out = append(out, fmt.Sprint(s))
	}
	return out
}

func TestBackupPlans(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	backupProject(t, "bk-shop", kwerftv1.ProjectSpec{})
	nightly := plan(t, "bk-nightly", kwerftv1.BackupPlanSpec{Scope: kwerftv1.BackupCluster, Schedule: "0 3 * * *"})

	sched := scheduleOf(t, "bk-nightly", func(spec, tpl map[string]any) error {
		ns := strs(tpl["includedNamespaces"])
		if !slices.Contains(ns, "bk-shop") || !slices.Contains(ns, GatewayNamespace) || !slices.Contains(ns, "kwerft-builds") {
			return fmt.Errorf("namespaces %v", ns)
		}
		return nil
	})
	spec := sched.Object["spec"].(map[string]any)
	tpl := spec["template"].(map[string]any)
	cluster := strs(tpl["includedClusterScopedResources"])
	if !slices.Contains(cluster, "projects.kwerft.dev") || !slices.Contains(cluster, "consolesettings.kwerft.dev") || slices.Contains(cluster, "restores.kwerft.dev") {
		t.Errorf("cluster resources %v", cluster)
	}
	if spec["schedule"] != "0 3 * * *" || spec["paused"] != false || tpl["ttl"] != "336h0m0s" || tpl["defaultVolumesToFsBackup"] != true ||
		tpl["storageLocation"] != BackupLocation || spec["skipImmediately"] != true {
		t.Errorf("schedule %v", spec)
	}
	if labels := tpl["metadata"].(map[string]any)["labels"].(map[string]any); labels[LabelBackupPlan] != "bk-nightly" || labels[LabelBackupScope] != "Cluster" {
		t.Errorf("backup labels %v", labels)
	}
	if !metav1.IsControlledBy(sched, nightly) {
		t.Errorf("schedule not controlled by its plan: %v", sched.GetOwnerReferences())
	}

	// The schedule follows new projects.
	backupProject(t, "bk-new", kwerftv1.ProjectSpec{})
	scheduleOf(t, "bk-nightly", func(_, tpl map[string]any) error {
		if !slices.Contains(strs(tpl["includedNamespaces"]), "bk-new") {
			return errors.New("bk-new not backed up")
		}
		return nil
	})

	// A Projects plan: its projects, objects only, paused.
	off := false
	plan(t, "bk-shop-only", kwerftv1.BackupPlanSpec{Scope: kwerftv1.BackupProjects, Projects: []string{"bk-shop"}, Schedule: "30 */6 * * *",
		Retention: &metav1.Duration{Duration: 72 * time.Hour}, Volumes: &off, Paused: true})
	scheduleOf(t, "bk-shop-only", func(spec, tpl map[string]any) error {
		if ns := strs(tpl["includedNamespaces"]); !slices.Equal(ns, []string{"bk-shop"}) {
			return fmt.Errorf("namespaces %v", ns)
		}
		if c := strs(tpl["includedClusterScopedResources"]); !slices.Equal(c, []string{"projects.kwerft.dev"}) {
			return fmt.Errorf("cluster resources %v", c)
		}
		if tpl["ttl"] != "72h0m0s" || tpl["defaultVolumesToFsBackup"] != false || spec["paused"] != true {
			return fmt.Errorf("spec %v", spec)
		}
		return nil
	})
	eventually(t, func() error {
		var p kwerftv1.BackupPlan
		if err := k8s.Get(ctx, client.ObjectKey{Name: "bk-shop-only"}, &p); err != nil {
			return err
		}
		if p.Status.ObservedGeneration != p.Generation || p.Status.NextRunAt != nil {
			return fmt.Errorf("paused plan status %+v", p.Status)
		}
		return nil
	})

	// "Back up now".
	eventually(t, func() error {
		var p kwerftv1.BackupPlan
		if err := k8s.Get(ctx, client.ObjectKey{Name: "bk-nightly"}, &p); err != nil {
			return err
		}
		p.Annotations = map[string]string{AnnotationRunRequested: "2026-10-05T10:00:00Z", AnnotationRequestedBy: "owner@example.com"}
		return k8s.Update(ctx, &p)
	})
	backupName := "kwerft-bk-nightly-20261005100000"
	var backup *unstructured.Unstructured
	eventually(t, func() error {
		var err error
		backup, err = getVelero(t, VeleroBackupGVK, backupName)
		return err
	})
	if l := backup.GetLabels(); l[LabelBackupPlan] != "bk-nightly" || l[LabelBackupScope] != "Cluster" || l[labelVeleroScheduleName] != "kwerft-bk-nightly" ||
		backup.GetAnnotations()[AnnotationRequestedBy] != "owner@example.com" {
		t.Errorf("backup metadata %v %v", l, backup.GetAnnotations())
	}
	if ns := strs(backup.Object["spec"].(map[string]any)["includedNamespaces"]); !slices.Contains(ns, "bk-shop") {
		t.Errorf("backup namespaces %v", ns)
	}
	eventually(t, func() error {
		var p kwerftv1.BackupPlan
		if err := k8s.Get(ctx, client.ObjectKey{Name: "bk-nightly"}, &p); err != nil {
			return err
		}
		if p.Annotations[annotationRunHandled] != "2026-10-05T10:00:00Z" {
			return errors.New("request not marked handled")
		}
		return nil
	})

	// Velero finishes it; then a scheduled one fails.
	setVeleroStatus(t, VeleroBackupGVK, backupName, map[string]any{"phase": "Completed",
		"startTimestamp": "2026-10-05T10:00:01Z", "completionTimestamp": "2026-10-05T10:04:00Z", "expiration": "2026-10-19T10:00:01Z",
		"progress": map[string]any{"itemsBackedUp": int64(412), "totalItems": int64(412)}, "warnings": int64(1)})
	waitForPlan(t, "bk-nightly", func(st *kwerftv1.BackupPlanStatus) error {
		if st.LastBackup == nil || st.LastBackup.Name != backupName || st.LastBackup.Phase != "Completed" || st.LastBackup.Items != 412 ||
			st.LastSuccessfulAt == nil || st.Backups != 1 || st.NextRunAt == nil {
			return fmt.Errorf("status %+v %+v", st, st.LastBackup)
		}
		return nil
	})
	failed := newVelero(VeleroBackupGVK)
	failed.SetNamespace(VeleroNamespace)
	failed.SetName("kwerft-bk-nightly-20261006030000")
	failed.SetLabels(map[string]string{LabelBackupPlan: "bk-nightly", LabelBackupScope: "Cluster"})
	failed.Object["spec"] = map[string]any{"includedNamespaces": []any{"bk-shop"}}
	failed.Object["status"] = map[string]any{"phase": "Failed", "startTimestamp": "2026-10-06T03:00:00Z", "failureReason": "bucket gone"}
	if err := k8s.Create(ctx, failed); err != nil {
		t.Fatal(err)
	}
	waitForPlan(t, "bk-nightly", func(st *kwerftv1.BackupPlanStatus) error {
		if st.LastBackup == nil || st.LastBackup.Phase != "Failed" || st.LastBackup.Message != "bucket gone" || st.Backups != 2 ||
			st.LastSuccessfulAt == nil || !st.LastSuccessfulAt.Time.Equal(time.Date(2026, 10, 5, 10, 4, 0, 0, time.UTC)) {
			return fmt.Errorf("status %+v %+v", st, st.LastBackup)
		}
		return nil
	})
	// Without a target the plan is not ready.
	var p kwerftv1.BackupPlan
	if err := k8s.Get(ctx, client.ObjectKey{Name: "bk-nightly"}, &p); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, ConditionReady); c == nil || c.Reason != "TargetNotReady" {
		t.Errorf("ready %+v", c)
	}
}

func waitForPlan(t *testing.T, name string, check func(*kwerftv1.BackupPlanStatus) error) {
	t.Helper()
	eventually(t, func() error {
		var p kwerftv1.BackupPlan
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &p); err != nil {
			return err
		}
		return check(&p.Status)
	})
}

// ---- restores ----------------------------------------------------------------------

func tarball(t *testing.T, files map[string]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, obj := range files {
		body, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// veleroBackup creates a completed backup holding namespaces.
func veleroBackup(t *testing.T, name, phase string, namespaces ...string) {
	t.Helper()
	b := newVelero(VeleroBackupGVK)
	b.SetNamespace(VeleroNamespace)
	b.SetName(name)
	b.Object["spec"] = map[string]any{"includedNamespaces": toAny(namespaces)}
	b.Object["status"] = map[string]any{"phase": phase}
	if err := k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), b) })
}

func restore(t *testing.T, name string, spec kwerftv1.RestoreSpec) {
	t.Helper()
	r := &kwerftv1.Restore{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), r) })
}

func waitForRestore(t *testing.T, name string, check func(*kwerftv1.RestoreStatus) error) {
	t.Helper()
	eventually(t, func() error {
		var r kwerftv1.Restore
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &r); err != nil {
			return err
		}
		return check(&r.Status)
	})
}

// serveContents plays Velero's DownloadRequest of a restore: a URL to the
// backup's tarball, signed for the location's SSE-C key, which the GET must
// send in headers (the storage answers 400 otherwise).
func serveContents(t *testing.T, restore string, body []byte) {
	t.Helper()
	key, err := backups.SSECustomerKey(backups.NewRecoveryKey())
	if err != nil {
		t.Fatal(err)
	}
	putSecret(t, VeleroNamespace, BackupEncryptionSecret, map[string][]byte{BackupEncryptionSecretKey: key})
	want := http.Header{}
	backups.SetSSEC(want, key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{backups.HeaderSSECAlgorithm, backups.HeaderSSECKey, backups.HeaderSSECKeyMD5} {
			if r.Header.Get(h) != want.Get(h) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("<Error><Code>InvalidRequest</Code></Error>"))
				return
			}
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	name := VeleroRestoreName(restore) + "-contents"
	eventually(t, func() error {
		dr, err := getVelero(t, VeleroDownloadRequestGVK, name)
		if err != nil {
			return err
		}
		if k := nestedString(dr, "spec", "target", "kind"); k != "BackupContents" {
			return fmt.Errorf("download of %s", k)
		}
		return nil
	})
	setVeleroStatus(t, VeleroDownloadRequestGVK, name, map[string]any{"phase": "Processed", "downloadURL": srv.URL + "/contents.tar.gz?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-SignedHeaders=" +
		"host%3Bx-amz-server-side-encryption-customer-algorithm%3Bx-amz-server-side-encryption-customer-key%3Bx-amz-server-side-encryption-customer-key-md5",
		"expiration": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)})
}

// putSecret creates or replaces a Secret's data.
func putSecret(t *testing.T, namespace, name string, data map[string][]byte) {
	t.Helper()
	eventually(t, func() error {
		var sec corev1.Secret
		err := k8s.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &sec)
		if err != nil {
			return k8s.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Data: data})
		}
		sec.Data = data
		return k8s.Update(context.Background(), &sec)
	})
}

func veleroRestoreOf(t *testing.T, restore string) map[string]any {
	t.Helper()
	var u *unstructured.Unstructured
	eventually(t, func() error {
		var err error
		u, err = getVelero(t, VeleroRestoreGVK, VeleroRestoreName(restore))
		return err
	})
	if u.GetLabels()[LabelRestore] != restore {
		t.Errorf("velero restore labels %v", u.GetLabels())
	}
	return u.Object["spec"].(map[string]any)
}

func TestRestoreProjectUnderNewName(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	backupProject(t, "rs-shop", kwerftv1.ProjectSpec{DisplayName: "Shop today"})
	veleroBackup(t, "kwerft-cluster-20261005030000", "Completed", "rs-shop", GatewayNamespace)
	restore(t, "rs-shop-copy", kwerftv1.RestoreSpec{Backup: "kwerft-cluster-20261005030000", Project: "rs-shop", TargetProject: "rs-copy"})
	t.Cleanup(func() { _ = k8s.Delete(ctx, &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "rs-copy"}}) })

	// The new project gets the original's spec as the backup holds it.
	serveContents(t, "rs-shop-copy", tarball(t, map[string]any{
		"resources/projects.kwerft.dev/cluster/rs-shop.json": kwerftv1.Project{
			TypeMeta:   metav1.TypeMeta{APIVersion: "kwerft.dev/v1alpha1", Kind: "Project"},
			ObjectMeta: metav1.ObjectMeta{Name: "rs-shop", UID: "old-uid", ResourceVersion: "42"},
			Spec:       kwerftv1.ProjectSpec{DisplayName: "Shop at backup time"}},
	}))
	var copied kwerftv1.Project
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Name: "rs-copy"}, &copied) })
	if copied.Spec.DisplayName != "Shop at backup time" || copied.Annotations[AnnotationRestoredBy] != "rs-shop-copy" {
		t.Errorf("project %+v %v", copied.Spec, copied.Annotations)
	}

	spec := veleroRestoreOf(t, "rs-shop-copy")
	if spec["backupName"] != "kwerft-cluster-20261005030000" || !slices.Equal(strs(spec["includedNamespaces"]), []string{"rs-shop"}) ||
		spec["existingResourcePolicy"] != "none" || spec["includeClusterResources"] != false ||
		spec["namespaceMapping"].(map[string]any)["rs-shop"] != "rs-copy" || spec["labelSelector"] != nil {
		t.Errorf("velero restore %v", spec)
	}
	waitForRestore(t, "rs-shop-copy", func(st *kwerftv1.RestoreStatus) error {
		if st.Phase != RestoreInProgress || st.VeleroRestore != "kwerft-rs-shop-copy" {
			return fmt.Errorf("status %+v", st)
		}
		return nil
	})
	setVeleroStatus(t, VeleroRestoreGVK, "kwerft-rs-shop-copy", map[string]any{"phase": "Completed", "warnings": int64(2),
		"completionTimestamp": "2026-10-05T12:00:00Z"})
	waitForRestore(t, "rs-shop-copy", func(st *kwerftv1.RestoreStatus) error {
		c := meta.FindStatusCondition(st.Conditions, ConditionReady)
		if st.Phase != RestoreCompleted || st.Warnings != 2 || st.CompletedAt == nil || c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("status %+v", st)
		}
		return nil
	})
}

func TestRestoreApps(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	backupProject(t, "rs-apps", kwerftv1.ProjectSpec{})
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Name: "rs-apps"}, &corev1.Namespace{}) })
	veleroBackup(t, "kwerft-apps-1", "PartiallyFailed", "rs-apps")
	restore(t, "rs-apps-web", kwerftv1.RestoreSpec{Backup: "kwerft-apps-1", Project: "rs-apps", Apps: []string{"web"}})

	tm := func(kind string) metav1.TypeMeta {
		return metav1.TypeMeta{APIVersion: "kwerft.dev/v1alpha1", Kind: kind}
	}
	om := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "rs-apps", Name: name, UID: "old", ResourceVersion: "7",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "X", Name: "gone", UID: "gone"}}}
	}
	serveContents(t, "rs-apps-web", tarball(t, map[string]any{
		"resources/apps.kwerft.dev/namespaces/rs-apps/web.json": kwerftv1.App{TypeMeta: tm("App"), ObjectMeta: om("web"), Spec: kwerftv1.AppSpec{
			Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1.27"}},
			Volumes: []kwerftv1.AppVolume{{Path: "/data", Volume: "shared"}},
			Env: []corev1.EnvVar{{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"}, Key: "password"}}}},
		}},
		"resources/apps.kwerft.dev/namespaces/rs-apps/worker.json": kwerftv1.App{TypeMeta: tm("App"), ObjectMeta: om("worker"),
			Spec: kwerftv1.AppSpec{Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "busybox"}}}},
		"resources/volumes.kwerft.dev/v1alpha1-preferredversion/namespaces/rs-apps/shared.json": kwerftv1.Volume{TypeMeta: tm("Volume"),
			ObjectMeta: om("shared"), Spec: kwerftv1.VolumeSpec{Size: resource.MustParse("5Gi")}},
		"resources/secrets/namespaces/rs-apps/db-creds.json": corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: om("db-creds"), Data: map[string][]byte{"password": []byte("hunter2")}},
	}))

	// What the App refers to comes first; the App once Velero is done.
	eventually(t, func() error {
		var v kwerftv1.Volume
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rs-apps", Name: "shared"}, &v); err != nil {
			return err
		}
		if len(v.OwnerReferences) > 0 || v.Spec.Size.String() != "5Gi" {
			return fmt.Errorf("volume %+v", v)
		}
		var s corev1.Secret
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rs-apps", Name: "db-creds"}, &s); err != nil {
			return err
		}
		if string(s.Data["password"]) != "hunter2" {
			return errors.New("secret data lost")
		}
		return nil
	})
	spec := veleroRestoreOf(t, "rs-apps-web")
	sel, _ := json.Marshal(spec["labelSelector"])
	if string(sel) != `{"matchExpressions":[{"key":"kwerft.dev/app","operator":"In","values":["web"]}]}` || spec["namespaceMapping"] != nil {
		t.Errorf("velero restore %v", spec)
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rs-apps", Name: "web"}, &kwerftv1.App{}); err == nil {
		t.Error("the App came before its workloads were restored")
	}
	setVeleroStatus(t, VeleroRestoreGVK, "kwerft-rs-apps-web", map[string]any{"phase": "PartiallyFailed", "errors": int64(1)})
	waitForRestore(t, "rs-apps-web", func(st *kwerftv1.RestoreStatus) error {
		if st.Phase != RestorePartiallyFailed || st.Errors != 1 {
			return fmt.Errorf("status %+v", st)
		}
		return nil
	})
	var app kwerftv1.App
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rs-apps", Name: "web"}, &app); err != nil {
		t.Fatal(err)
	}
	if app.Spec.Source.Image.Ref != "nginx:1.27" || len(app.OwnerReferences) > 0 {
		t.Errorf("app %+v", app)
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rs-apps", Name: "worker"}, &kwerftv1.App{}); err == nil {
		t.Error("an app that was not asked for came back")
	}
}

func TestRestoreRefused(t *testing.T) {
	requireEnvtest(t)
	backupProject(t, "rs-other", kwerftv1.ProjectSpec{})
	veleroBackup(t, "kwerft-other-1", "Completed", "rs-elsewhere")
	veleroBackup(t, "kwerft-other-2", "InProgress", "rs-other")
	for name, c := range map[string]struct {
		spec kwerftv1.RestoreSpec
		want string
	}{
		"rs-missing":     {kwerftv1.RestoreSpec{Backup: "nope", Project: "rs-other"}, "does not exist"},
		"rs-not-held":    {kwerftv1.RestoreSpec{Backup: "kwerft-other-1", Project: "rs-other"}, "does not hold the project"},
		"rs-in-progress": {kwerftv1.RestoreSpec{Backup: "kwerft-other-2", Project: "rs-other"}, "only completed backups"},
	} {
		restore(t, name, c.spec)
		waitForRestore(t, name, func(st *kwerftv1.RestoreStatus) error {
			if st.Phase != RestoreFailed || !strings.Contains(st.Message, c.want) || st.CompletedAt == nil {
				return fmt.Errorf("%s: %+v", name, st)
			}
			return nil
		})
	}
	// An App the backup does not hold.
	veleroBackup(t, "kwerft-other-3", "Completed", "rs-other")
	restore(t, "rs-no-app", kwerftv1.RestoreSpec{Backup: "kwerft-other-3", Project: "rs-other", Apps: []string{"ghost"}})
	serveContents(t, "rs-no-app", tarball(t, map[string]any{}))
	waitForRestore(t, "rs-no-app", func(st *kwerftv1.RestoreStatus) error {
		if st.Phase != RestoreFailed || !strings.Contains(st.Message, "app ghost") {
			return fmt.Errorf("%+v", st)
		}
		return nil
	})
}

// Every cluster-scoped kind of Kwerft is either in a Cluster backup or
// deliberately left out.
func TestBackupCoversEveryClusterKind(t *testing.T) {
	requireEnvtest(t)
	var crds unstructured.UnstructuredList
	crds.SetAPIVersion("apiextensions.k8s.io/v1")
	crds.SetKind("CustomResourceDefinitionList")
	if err := k8s.List(context.Background(), &crds); err != nil {
		t.Fatal(err)
	}
	for _, crd := range crds.Items {
		if nestedString(&crd, "spec", "group") != "kwerft.dev" || nestedString(&crd, "spec", "scope") != "Cluster" {
			continue
		}
		if name := crd.GetName(); !slices.Contains(BackupClusterResources, name) && !slices.Contains(BackupSkippedResources, name) {
			t.Errorf("%s is neither backed up nor skipped", name)
		}
	}
}
