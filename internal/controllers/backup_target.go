package controllers

import (
	"cmp"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/backups"
)

// The backup target: ConsoleSettings.spec.backups with the access keys
// (kwerft-backup-credentials) and the recovery key (kwerft-backup-key)
// become, in the namespace velero, the Secret kwerft-bsl-credentials, the
// Kopia repository password velero-repo-credentials and the default
// BackupStorageLocation "kwerft" (prefix <prefix>/velero); with etcd
// snapshots on, also k3s's S3 configuration kube-system/kwerft-etcd-s3
// (folder <prefix>/etcd). status.backups reports the location's state.
//
// The reconciler also creates the two write-only Secrets empty, so owners
// and admins can fill them with "patch" alone (roles.yaml); the recovery
// key's carries velero.io/exclude-from-backup, so it is never stored next to
// the data it unlocks.

const (
	backupTargetResync = 10 * time.Minute
	backupTargetPoll   = 30 * time.Second
)

var backupTargetRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "backup-target"}}

// BackupTargetReconciler: see above.
type BackupTargetReconciler struct {
	client.Client
	// APIReader reads the Secrets uncached; nil falls back to the client.
	APIReader client.Reader
	// ConsoleDomain is the --console-domain flag: the prefix when the
	// settings name none (the API writes one on the first save).
	ConsoleDomain string
	Now           func() time.Time
}

func (r *BackupTargetReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *BackupTargetReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// BackupPrefix is the prefix a target uses: its own, else the console's
// hostname.
func BackupPrefix(b *kwerftv1.BackupSettings, consoleDomain string) string {
	if p := strings.Trim(b.Prefix, "/"); p != "" {
		return p
	}
	return consoleDomain
}

func (r *BackupTargetReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := r.ensureSecrets(ctx); err != nil {
		return ctrl.Result{}, err
	}
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	st := &kwerftv1.BackupsStatus{}
	if prev := s.Status.Backups; prev != nil {
		st.RecoveryKeyCreatedAt = prev.RecoveryKeyCreatedAt
	}
	if at, err := time.Parse(time.RFC3339Nano, s.Annotations[AnnotationBackupKeyCreated]); err == nil {
		st.RecoveryKeyCreatedAt = &metav1.Time{Time: at.Truncate(time.Second)}
	}
	last, err := r.lastSuccess(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.LastSuccessfulAt = last

	b := s.Spec.Backups
	creds, err := r.secretData(ctx, GatewayNamespace, BackupCredentialsSecret)
	if err != nil {
		return ctrl.Result{}, err
	}
	keyData, err := r.secretData(ctx, GatewayNamespace, BackupKeySecret)
	if err != nil {
		return ctrl.Result{}, err
	}
	access, secret := strings.TrimSpace(string(creds[BackupAccessKeyKey])), strings.TrimSpace(string(creds[BackupSecretKeyKey]))
	key := strings.TrimSpace(string(keyData[BackupKeySecretKey]))
	if key == "" && b != nil {
		// A console restored by install.sh --restore: the recovery key is
		// never in a backup, but the installer wrote it as Velero's
		// repository password.
		if key, err = r.adoptKey(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}
	switch {
	case b == nil:
		st.State, st.Message = "NotConfigured", "No backup target is set. Enter one under Settings › Backups."
	case access == "" || secret == "":
		st.State, st.Message = "NotConfigured", "The bucket's access keys are missing. Enter them under Settings › Backups."
	case key == "":
		st.State, st.Message = "NotConfigured", "The recovery key is missing. Save the target again under Settings › Backups to create one, or enter the key of earlier backups."
	}
	if st.State != "" {
		return ctrl.Result{RequeueAfter: backupTargetResync}, r.report(ctx, &s, st)
	}
	password, err := backups.RepositoryPassword(key)
	if err != nil {
		st.State, st.Message = "Error", "The stored recovery key is not one Kwerft made (52 letters and digits). Enter the key of earlier backups under Settings › Backups."
		return ctrl.Result{RequeueAfter: backupTargetResync}, r.report(ctx, &s, st)
	}
	prefix := BackupPrefix(b, ConsoleHost(ctx, r.Client, r.ConsoleDomain))
	after, err := r.applyVelero(ctx, b, prefix, access, secret, password, st)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyEtcd(ctx, b, prefix, access, secret); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, r.report(ctx, &s, st)
}

// ensureSecrets creates the empty write-only Secrets and keeps the
// recovery key's out of backups.
func (r *BackupTargetReconciler) ensureSecrets(ctx context.Context) error {
	for _, name := range []string{BackupCredentialsSecret, BackupKeySecret} {
		labels := map[string]string{LabelManagedBy: ManagedByKwerft}
		if name == BackupKeySecret {
			labels[labelVeleroExclude] = "true"
		}
		var sec corev1.Secret
		err := r.reader().Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: name}, &sec)
		switch {
		case apierrors.IsNotFound(err):
			sec = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: GatewayNamespace, Labels: labels}, Type: corev1.SecretTypeOpaque}
			if err := r.Create(ctx, &sec); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
		case err != nil:
			return err
		case sec.Labels[labelVeleroExclude] != labels[labelVeleroExclude] && name == BackupKeySecret:
			patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{labelVeleroExclude: "true"}}})
			if err := r.Patch(ctx, &sec, client.RawPatch(types.MergePatchType, patch)); err != nil {
				return err
			}
		}
	}
	return nil
}

// adoptKey takes the recovery key from Velero's repository password when
// that is one (not Velero's built-in default), and stores it in
// kwerft-backup-key; "" when there is none.
func (r *BackupTargetReconciler) adoptKey(ctx context.Context) (string, error) {
	repo, err := r.secretData(ctx, VeleroNamespace, RepoPasswordSecret)
	if err != nil {
		return "", err
	}
	key, err := backups.NormalizeRecoveryKey(string(repo[RepoPasswordSecretKey]))
	if err != nil {
		return "", nil
	}
	patch, _ := json.Marshal(map[string]any{"data": map[string][]byte{BackupKeySecretKey: []byte(key)}})
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: GatewayNamespace, Name: BackupKeySecret}}
	if err := r.Patch(ctx, sec, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).Info("took the recovery key from Velero's repository password")
	return key, nil
}

func (r *BackupTargetReconciler) secretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	var sec corev1.Secret
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return sec.Data, err
}

// lastSuccess is the newest successful backup of any plan.
func (r *BackupTargetReconciler) lastSuccess(ctx context.Context) (*metav1.Time, error) {
	var plans kwerftv1.BackupPlanList
	if err := r.List(ctx, &plans); err != nil {
		return nil, err
	}
	var last *metav1.Time
	for _, p := range plans.Items {
		if t := p.Status.LastSuccessfulAt; t != nil && (last == nil || t.After(last.Time)) {
			last = t.DeepCopy()
		}
	}
	return last, nil
}

// credentialsFile is the AWS credentials file Velero's AWS plugin reads.
func credentialsFile(access, secret string) string {
	return "[default]\naws_access_key_id=" + access + "\naws_secret_access_key=" + secret + "\n"
}

func (r *BackupTargetReconciler) applyVelero(ctx context.Context, b *kwerftv1.BackupSettings, prefix, access, secret, password string,
	st *kwerftv1.BackupsStatus) (time.Duration, error) {
	noVelero := func() (time.Duration, error) {
		st.State, st.Message = "Error", "Velero is not installed: re-run the installer (stage Backups; --lite leaves it out)."
		return time.Minute, nil
	}
	secrets := []*corev1ac.SecretApplyConfiguration{
		corev1ac.Secret(BackupLocationSecret, VeleroNamespace).
			WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
			WithType(corev1.SecretTypeOpaque).
			WithData(map[string][]byte{BackupLocationSecretKey: []byte(credentialsFile(access, secret))}),
		// Velero creates it with a default password when it starts; no
		// repository uses that one, since the console makes the only
		// location, and only after this Secret. Its other keys stay.
		corev1ac.Secret(RepoPasswordSecret, VeleroNamespace).
			WithData(map[string][]byte{RepoPasswordSecretKey: []byte(password)}),
	}
	for _, sec := range secrets {
		err := apply(ctx, r.Client, sec)
		if apierrors.IsNotFound(err) {
			return noVelero()
		}
		if err != nil {
			return 0, err
		}
	}
	region := backups.RegionFor(backups.Target{Endpoint: b.Endpoint, Region: b.Region})
	bsl := newVelero(VeleroBSLGVK)
	bsl.SetName(BackupLocation)
	bsl.SetNamespace(VeleroNamespace)
	bsl.SetLabels(map[string]string{LabelManagedBy: ManagedByKwerft})
	bsl.Object["spec"] = map[string]any{
		"provider":      "aws",
		"default":       true,
		"accessMode":    "ReadWrite",
		"objectStorage": map[string]any{"bucket": b.Bucket, "prefix": prefix + "/velero"},
		"config": map[string]any{
			"region": region, "s3Url": strings.TrimRight(b.Endpoint, "/"), "s3ForcePathStyle": "true",
			// S3-compatible stores (Hetzner's among them) reject the AWS
			// SDK's default CRC32 checksums.
			"checksumAlgorithm": "",
		},
		"credential": map[string]any{"name": BackupLocationSecret, "key": BackupLocationSecretKey},
	}
	err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(bsl), client.FieldOwner(FieldOwner), client.ForceOwnership)
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return noVelero()
	}
	if err != nil {
		return 0, err
	}
	got := newVelero(VeleroBSLGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: BackupLocation}, got); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	if t := nestedTime(got, "status", "lastValidationTime"); t != nil {
		st.CheckedAt = &metav1.Time{Time: *t}
	}
	switch nestedString(got, "status", "phase") {
	case "Available":
		st.State = "Ready"
		return backupTargetResync, nil
	case "Unavailable":
		st.State = "Error"
		st.Message = "Velero cannot use the bucket"
		if msg := nestedString(got, "status", "message"); msg != "" {
			st.Message += ": " + msg
		}
		return time.Minute, nil
	default:
		st.State, st.Message = "Pending", "Velero is checking the bucket."
		return backupTargetPoll, nil
	}
}

// applyEtcd keeps k3s's etcd snapshot S3 configuration: with a bucket set,
// etcd snapshots always go there too (spec.backups.etcdSnapshots nil means
// the installer's defaults, every 6 hours and 28 kept).
func (r *BackupTargetReconciler) applyEtcd(ctx context.Context, b *kwerftv1.BackupSettings, prefix, access, secret string) error {
	return apply(ctx, r.Client, corev1ac.Secret(EtcdS3Secret, etcdS3Namespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
		WithType(etcdS3Type).
		WithData(EtcdS3Config(b, prefix, access, secret)))
}

// EtcdS3Config is the k3s etcd S3 configuration Secret's data: its keys
// are the --etcd-s3-* flags.
func EtcdS3Config(b *kwerftv1.BackupSettings, prefix, access, secret string) map[string][]byte {
	host := strings.TrimRight(strings.TrimPrefix(b.Endpoint, "https://"), "/")
	if u, err := url.Parse(b.Endpoint); err == nil && u.Host != "" {
		host = u.Host
	}
	out := map[string]string{
		"etcd-s3-endpoint":        host,
		"etcd-s3-access-key":      access,
		"etcd-s3-secret-key":      secret,
		"etcd-s3-bucket":          b.Bucket,
		"etcd-s3-folder":          prefix + "/etcd",
		"etcd-s3-region":          backups.RegionFor(backups.Target{Endpoint: b.Endpoint, Region: b.Region}),
		"etcd-s3-insecure":        "false",
		"etcd-s3-skip-ssl-verify": "false",
		"etcd-s3-timeout":         "5m",
		// The installer's schedule keeps this many; without it k3s prunes
		// the bucket by its local retention.
		"etcd-s3-retention": strconv.Itoa(int(cmp.Or(retention(b.EtcdSnapshots), DefaultEtcdRetention))),
	}
	data := make(map[string][]byte, len(out))
	for k, v := range out {
		data[k] = []byte(v)
	}
	return data
}

// DefaultEtcdRetention is how many etcd snapshots are kept when the
// settings name no number (install.sh's default).
const DefaultEtcdRetention = 28

func retention(e *kwerftv1.EtcdSnapshotSettings) int32 {
	if e == nil {
		return 0
	}
	return e.Retention
}

func (r *BackupTargetReconciler) report(ctx context.Context, s *kwerftv1.ConsoleSettings, st *kwerftv1.BackupsStatus) error {
	// Velero validates the location every minute; a new check time alone
	// is written at most hourly.
	if prev := s.Status.Backups; prev != nil {
		same := st.DeepCopy()
		same.CheckedAt = prev.CheckedAt
		fresh := prev.CheckedAt == nil && st.CheckedAt == nil ||
			prev.CheckedAt != nil && st.CheckedAt != nil && r.now().Sub(prev.CheckedAt.Time) < time.Hour
		if equality.Semantic.DeepEqual(prev, same) && fresh {
			return nil
		}
	}
	orig := s.DeepCopy()
	s.Status.Backups = st
	if err := patchStatus(ctx, r.Client, s, orig); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// backupTargetInputs is what in the settings makes the reconciler act.
func backupTargetInputs(s *kwerftv1.ConsoleSettings) string {
	raw, _ := json.Marshal([]any{s.Spec.Backups, s.Annotations[AnnotationBackupCredentialsUpdated], s.Annotations[AnnotationBackupKeyCreated],
		s.Status.ConsoleDomain})
	return string(raw)
}

func (r *BackupTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{backupTargetRequest}
	})
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}}}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("backup-target").
		WatchesRawSource(source.Channel(start, all)).
		Watches(&kwerftv1.ConsoleSettings{}, all, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return backupTargetInputs(e.ObjectOld.(*kwerftv1.ConsoleSettings)) != backupTargetInputs(e.ObjectNew.(*kwerftv1.ConsoleSettings))
			},
		})).
		// The newest successful backup of any plan.
		Watches(&kwerftv1.BackupPlan{}, all, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return !equality.Semantic.DeepEqual(e.ObjectOld.(*kwerftv1.BackupPlan).Status.LastSuccessfulAt,
					e.ObjectNew.(*kwerftv1.BackupPlan).Status.LastSuccessfulAt)
			},
		}))
	// Velero's verdict on the bucket, where Velero is installed (otherwise
	// the reconciler polls).
	if veleroInstalled(mgr, VeleroBSLGVK) {
		b = b.Watches(newVelero(VeleroBSLGVK), all, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetNamespace() == VeleroNamespace && o.GetName() == BackupLocation
		}), veleroPhaseChanged()))
	}
	return b.Complete(r)
}

// veleroInstalled reports whether the cluster serves a Velero kind.
func veleroInstalled(mgr ctrl.Manager, gvk schema.GroupVersionKind) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil
}

// veleroPhaseChanged passes updates that change an object's status.phase
// or message (not Velero's periodic timestamps).
func veleroPhaseChanged() predicate.Funcs {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		o, n := e.ObjectOld.(*unstructured.Unstructured), e.ObjectNew.(*unstructured.Unstructured)
		return nestedString(o, "status", "phase") != nestedString(n, "status", "phase") ||
			nestedString(o, "status", "message") != nestedString(n, "status", "message") ||
			o.GetGeneration() != n.GetGeneration()
	}}
}
