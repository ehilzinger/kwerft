package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpgradeComponent is what an Upgrade moves to a new version.
// +kubebuilder:validation:Enum=Kwerft;Kubernetes
type UpgradeComponent string

const (
	UpgradeKwerft     UpgradeComponent = "Kwerft"
	UpgradeKubernetes UpgradeComponent = "Kubernetes"
)

// UpgradePhase is where an Upgrade stands (docs/phase6-upgrades.md).
type UpgradePhase string

const (
	UpgradeQueued      UpgradePhase = "Queued"
	UpgradePreflight   UpgradePhase = "Preflight"
	UpgradeBackingUp   UpgradePhase = "Backup"
	UpgradeRunning     UpgradePhase = "Running"
	UpgradeVerifying   UpgradePhase = "Verifying"
	UpgradeSucceeded   UpgradePhase = "Succeeded"
	UpgradeRollingBack UpgradePhase = "RollingBack"
	UpgradeRolledBack  UpgradePhase = "RolledBack"
	UpgradeFailed      UpgradePhase = "Failed"
	UpgradeCancelled   UpgradePhase = "Cancelled"
)

// UpgradeSpec is one attempt to move Kwerft or k3s to a version. The API
// creates it as the owner who asked (annotation kwerft.dev/requested-by);
// the update policy creates AutoPatch ones as "auto-update".
type UpgradeSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="component cannot be changed"
	Component UpgradeComponent `json:"component"`

	// Version is a Kwerft release (0.6.0) or a k3s version (v1.38.1+k3s1).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="version cannot be changed"
	Version string `json:"version"`

	// AcceptDataRollback acknowledges that rolling back a release marked
	// rollbackSafe: false restores the database copy, losing writes made
	// since the backup.
	// +optional
	AcceptDataRollback bool `json:"acceptDataRollback,omitempty"`
}

// UpgradeVersions are the running versions before an Upgrade.
type UpgradeVersions struct {
	// +optional
	Kwerft string `json:"kwerft,omitempty"`
	// +optional
	Kubernetes string `json:"kubernetes,omitempty"`
}

// UpgradeCheck is one preflight check.
type UpgradeCheck struct {
	Check string `json:"check"`
	OK    bool   `json:"ok"`
	// +optional
	Message string `json:"message,omitempty"`
	// Warning: a failed check that does not block (e.g. deprecated APIs on
	// a patch upgrade).
	// +optional
	Warning bool `json:"warning,omitempty"`
}

// UpgradeBackup is what the upgrade can roll back to.
type UpgradeBackup struct {
	// +optional
	EtcdSnapshot string `json:"etcdSnapshot,omitempty"`
	// Database is the path of the SQLite copy in the console's volume.
	// +optional
	Database string `json:"database,omitempty"`
	// HelmRevisions maps "<namespace>/<release>" to its revision before the
	// upgrade.
	// +optional
	HelmRevisions map[string]int32 `json:"helmRevisions,omitempty"`
}

// UpgradeStep is one installer stage, with its summary line as printed.
type UpgradeStep struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// State: Running, Done, Skipped or Failed.
	State string `json:"state"`
	// +optional
	Detail string `json:"detail,omitempty"`
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// UpgradeNode is one node's progress in a Kubernetes upgrade.
type UpgradeNode struct {
	Name string `json:"name"`
	// +optional
	Version string `json:"version,omitempty"`
	// State: Waiting, Draining, Upgrading, Done or Failed.
	State string `json:"state"`
	// +optional
	Message string `json:"message,omitempty"`
}

// UpgradeStatus is written by the Upgrade controller and the runner.
type UpgradeStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase UpgradePhase `json:"phase,omitempty"`
	// +optional
	From *UpgradeVersions `json:"from,omitempty"`
	// +optional
	Preflight []UpgradeCheck `json:"preflight,omitempty"`
	// +optional
	Backup *UpgradeBackup `json:"backup,omitempty"`
	// +optional
	Steps []UpgradeStep `json:"steps,omitempty"`
	// +optional
	Nodes []UpgradeNode `json:"nodes,omitempty"`
	// Reason of a failure: Usage, Preflight, Network, Kubernetes, Platform,
	// Kwerft (the installer's exit codes), Verify or Timeout.
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Upgrade is one attempt to upgrade Kwerft or Kubernetes on this cluster.
// Cluster-scoped; at most one is active at a time, others wait as Queued.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Component",type=string,JSONPath=`.spec.component`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Upgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpgradeSpec   `json:"spec"`
	Status UpgradeStatus `json:"status,omitempty"`
}

// UpgradeList contains a list of Upgrades.
//
// +kubebuilder:object:root=true
type UpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Upgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Upgrade{}, &UpgradeList{})
}
