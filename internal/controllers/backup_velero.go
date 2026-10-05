package controllers

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

// Backups (docs/phase6.md › Backups): Velero, installed by install.sh in
// the namespace velero without a BackupStorageLocation, does the work. The
// console keeps Velero's objects from Kwerft's:
//
//   - ConsoleSettings.spec.backups and the write-only Secrets
//     kwerft-backup-credentials and kwerft-backup-key → the
//     BackupStorageLocation "kwerft", its credentials and the Kopia
//     repository password (backup_target.go), plus the k3s etcd snapshot
//     S3 Secret;
//   - BackupPlan → Schedule kwerft-<plan>; "Back up now" → a Backup from it
//     (backup_plan.go);
//   - Restore → a Velero Restore of one project (backup_restore.go).
//
// Velero's objects are unstructured: Kwerft does not import Velero's API
// module. The CRDs of the Velero release install.sh pins are vendored for
// the tests under testdata/crds (velero.io_*.yaml).

var (
	VeleroBackupGVK          = schema.GroupVersionKind{Group: "velero.io", Version: "v1", Kind: "Backup"}
	VeleroScheduleGVK        = schema.GroupVersionKind{Group: "velero.io", Version: "v1", Kind: "Schedule"}
	VeleroRestoreGVK         = schema.GroupVersionKind{Group: "velero.io", Version: "v1", Kind: "Restore"}
	VeleroBSLGVK             = schema.GroupVersionKind{Group: "velero.io", Version: "v1", Kind: "BackupStorageLocation"}
	VeleroDownloadRequestGVK = schema.GroupVersionKind{Group: "velero.io", Version: "v1", Kind: "DownloadRequest"}
	VeleroPodVolumeBackupGVK = schema.GroupVersionKind{Group: "velero.io", Version: "v1", Kind: "PodVolumeBackup"}
)

const (
	// VeleroNamespace is where install.sh installs Velero.
	VeleroNamespace = "velero"
	// BackupLocation is the console's BackupStorageLocation (the default).
	BackupLocation = "kwerft"
	// BackupLocationSecret holds the location's access keys in the AWS
	// credentials file format (key "cloud"), in the velero namespace.
	BackupLocationSecret    = "kwerft-bsl-credentials"
	BackupLocationSecretKey = "cloud"
	// RepoPasswordSecret is Velero's own name for the Kopia repository
	// password (key "repository-password"): the recovery key.
	RepoPasswordSecret    = "velero-repo-credentials"
	RepoPasswordSecretKey = "repository-password"
	// BackupEncryptionSecret holds the SSE-C key Velero's AWS plugin
	// encrypts every object it writes with (key "sse-c-key": the 32 bytes
	// backups.SSECustomerKey derives from the recovery key), in the velero
	// namespace, which no backup includes. The location names it in
	// config.customerKeyEncryptionSecret ("<secret>/<key>"; the plugin
	// reads it through the API at every use).
	BackupEncryptionSecret    = "kwerft-bsl-encryption"
	BackupEncryptionSecretKey = "sse-c-key"
	// EtcdS3Secret is k3s's etcd snapshot S3 configuration
	// (--etcd-s3-config-secret, which install.sh sets), in kube-system.
	EtcdS3Secret    = "kwerft-etcd-s3"
	etcdS3Namespace = "kube-system"
	etcdS3Type      = "etcd.k3s.cattle.io/s3-config-secret"

	// BackupCredentialsSecret holds the bucket's access keys (accessKey,
	// secretKey) and BackupKeySecret the recovery key (key), both in
	// kwerft-system and write-only: Kwerft creates them empty, owners and
	// admins patch them, nobody reads them back.
	BackupCredentialsSecret = "kwerft-backup-credentials"
	BackupAccessKeyKey      = "accessKey"
	BackupSecretKeyKey      = "secretKey"
	BackupKeySecret         = "kwerft-backup-key"
	BackupKeySecretKey      = "key"
	// AnnotationBackupCredentialsUpdated on ConsoleSettings: when the
	// access keys were last written (Settings shows whether they are set; a
	// change makes the reconciler pass them on).
	AnnotationBackupCredentialsUpdated = "kwerft.dev/backup-credentials-updated-at"
	// AnnotationBackupKeyCreated on ConsoleSettings: when the recovery key
	// was stored. Set once; the key never changes in v1.
	AnnotationBackupKeyCreated = "kwerft.dev/backup-key-created-at"

	// LabelBackupPlan and LabelBackupScope are on a plan's Schedule and on
	// every Backup made from it.
	LabelBackupPlan  = "kwerft.dev/backup-plan"
	LabelBackupScope = "kwerft.dev/backup-scope"
	// AnnotationRunRequested on a BackupPlan asks for a backup now ("Back
	// up now"); the value is the request's time (RFC 3339). The reconciler
	// records the handled value in annotationRunHandled.
	AnnotationRunRequested = "kwerft.dev/run-requested"
	annotationRunHandled   = "kwerft.dev/run-handled"
	// AnnotationRequestedBy names the console user behind a Restore or a
	// "Back up now" (on the plan, copied to the Backup).
	AnnotationRequestedBy = "kwerft.dev/requested-by"
	// LabelRestore is on the Velero Restore (and DownloadRequest) of a
	// Restore.
	LabelRestore = "kwerft.dev/restore"
	// AnnotationRestoredBy on a Project a Restore created.
	AnnotationRestoredBy = "kwerft.dev/restored-by"

	// labelVeleroExclude keeps an object out of every backup (the recovery
	// key must never be stored next to the data it unlocks).
	labelVeleroExclude      = "velero.io/exclude-from-backup"
	labelVeleroScheduleName = "velero.io/schedule-name"

	// DefaultBackupRetention is a plan's retention when it names none.
	DefaultBackupRetention = 14 * 24 * time.Hour
	// DefaultBackupPlan is created on the first save of the target.
	DefaultBackupPlan     = "cluster"
	DefaultBackupSchedule = "0 3 * * *"
)

// BackupPlatformNamespaces are Kwerft's own namespaces a Cluster backup
// holds besides every project's: the console (database volume, data key,
// tokens, the Gateway's certificates) and the builds (zot's images, which
// pinned revisions point at).
var BackupPlatformNamespaces = []string{GatewayNamespace, builds.Namespace}

// BackupClusterResources are the cluster-scoped kinds a Cluster backup
// holds: Kwerft's, never Restores or Upgrades, which would act again when
// restored. Their CRDs come from the chart (install.sh --restore applies
// them before the restore). Nodes, the observability stack, cert-manager
// and Velero itself are not backed up.
var BackupClusterResources = []string{
	"alertrules.kwerft.dev", "backupplans.kwerft.dev", "clusters.kwerft.dev", "consolesettings.kwerft.dev",
	"firewallrules.kwerft.dev", "gitconnections.kwerft.dev", "nodepools.kwerft.dev", "notificationchannels.kwerft.dev",
	"projects.kwerft.dev",
}

// BackupSkippedResources are Kwerft's cluster-scoped kinds no backup holds.
var BackupSkippedResources = []string{"restores.kwerft.dev", "upgrades.kwerft.dev"}

// backupCronParser reads plan schedules as Velero does (five fields, UTC).
var backupCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseBackupSchedule checks a plan's cron expression.
func ParseBackupSchedule(spec string) (cron.Schedule, error) {
	return backupCronParser.Parse(strings.TrimSpace(spec))
}

// BackupInterval is the time between two runs of a schedule after t
// (the alert "no backup within twice the interval" uses it).
func BackupInterval(s cron.Schedule, t time.Time) time.Duration {
	a := s.Next(t)
	return s.Next(a).Sub(a)
}

// PlanRetention is the plan's retention with the default filled in.
func PlanRetention(p *kwerftv1.BackupPlan) time.Duration {
	if p.Spec.Retention != nil && p.Spec.Retention.Duration > 0 {
		return p.Spec.Retention.Duration
	}
	return DefaultBackupRetention
}

// PlanVolumes reports whether the plan backs up volume data.
func PlanVolumes(p *kwerftv1.BackupPlan) bool { return p.Spec.Volumes == nil || *p.Spec.Volumes }

// PlanScope is the plan's scope with the default filled in.
func PlanScope(p *kwerftv1.BackupPlan) kwerftv1.BackupScope {
	return cmp.Or(p.Spec.Scope, kwerftv1.BackupCluster)
}

// BackupScheduleName is the Velero Schedule of a plan.
func BackupScheduleName(plan string) string { return "kwerft-" + plan }

// backupTemplate is the Velero BackupSpec of a plan. projects are the
// cluster's projects (a Cluster plan backs up all of them).
func backupTemplate(p *kwerftv1.BackupPlan, projects []string) map[string]any {
	scope := PlanScope(p)
	var namespaces, clusterResources []string
	if scope == kwerftv1.BackupCluster {
		namespaces = append(slices.Clone(projects), BackupPlatformNamespaces...)
		clusterResources = slices.Clone(BackupClusterResources)
	} else {
		namespaces = slices.Clone(p.Spec.Projects)
		// The projects' own objects; a restore under a new name copies
		// their spec from here.
		clusterResources = []string{"projects.kwerft.dev"}
	}
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	return map[string]any{
		"metadata":                         map[string]any{"labels": stringMap(backupLabels(p))},
		"includedNamespaces":               toAny(namespaces),
		"includedNamespaceScopedResources": []any{"*"},
		// Events say nothing a restore needs.
		"excludedNamespaceScopedResources": []any{"events", "events.events.k8s.io"},
		"includedClusterScopedResources":   toAny(clusterResources),
		"defaultVolumesToFsBackup":         PlanVolumes(p),
		"snapshotVolumes":                  false,
		"storageLocation":                  BackupLocation,
		"ttl":                              PlanRetention(p).String(),
	}
}

func backupLabels(p *kwerftv1.BackupPlan) map[string]string {
	return map[string]string{LabelBackupPlan: p.Name, LabelBackupScope: string(PlanScope(p))}
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// ---- reading Velero objects --------------------------------------------------------

func newVelero(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

func newVeleroList(gvk schema.GroupVersionKind) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	return l
}

func nestedString(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

func nestedInt(u *unstructured.Unstructured, fields ...string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(u.Object, fields...)
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

func nestedTime(u *unstructured.Unstructured, fields ...string) *time.Time {
	s := nestedString(u, fields...)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// VeleroBackup is what the console shows of a Velero Backup.
type VeleroBackup struct {
	Name        string
	Plan        string
	Scope       string
	Phase       string
	Namespaces  []string
	Created     time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	ExpiresAt   *time.Time
	Items       int64
	TotalItems  int64
	Warnings    int64
	Errors      int64
	Message     string
	Volumes     bool
}

// ReadVeleroBackup reads the fields the console uses.
func ReadVeleroBackup(u *unstructured.Unstructured) VeleroBackup {
	b := VeleroBackup{
		Name: u.GetName(), Plan: u.GetLabels()[LabelBackupPlan], Scope: u.GetLabels()[LabelBackupScope],
		Phase: cmp.Or(nestedString(u, "status", "phase"), "New"), Created: u.GetCreationTimestamp().Time,
		StartedAt: nestedTime(u, "status", "startTimestamp"), CompletedAt: nestedTime(u, "status", "completionTimestamp"),
		ExpiresAt: nestedTime(u, "status", "expiration"),
		Items:     nestedInt(u, "status", "progress", "itemsBackedUp"), TotalItems: nestedInt(u, "status", "progress", "totalItems"),
		Warnings: nestedInt(u, "status", "warnings"), Errors: nestedInt(u, "status", "errors"),
		Message: nestedString(u, "status", "failureReason"),
	}
	b.Namespaces, _, _ = unstructured.NestedStringSlice(u.Object, "spec", "includedNamespaces")
	b.Volumes, _, _ = unstructured.NestedBool(u.Object, "spec", "defaultVolumesToFsBackup")
	if errs, _, _ := unstructured.NestedStringSlice(u.Object, "status", "validationErrors"); b.Message == "" && len(errs) > 0 {
		b.Message = strings.Join(errs, "; ")
	}
	return b
}

// Started is when the backup started, or was created before that.
func (b VeleroBackup) Started() time.Time {
	if b.StartedAt != nil {
		return *b.StartedAt
	}
	return b.Created
}

// BackupFailed reports whether a Velero phase is a failure.
func BackupFailed(phase string) bool {
	switch phase {
	case "Failed", "PartiallyFailed", "FailedValidation", "FinalizingPartiallyFailed", "WaitingForPluginOperationsPartiallyFailed":
		return true
	}
	return false
}

// BackupRestorable reports whether a backup can be restored from.
func BackupRestorable(phase string) bool { return phase == "Completed" || phase == "PartiallyFailed" }

// BackupHolds reports whether a backup holds a namespace.
func BackupHolds(b VeleroBackup, namespace string) bool {
	return slices.Contains(b.Namespaces, namespace) || slices.Contains(b.Namespaces, "*")
}
