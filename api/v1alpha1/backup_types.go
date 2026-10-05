package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Backups (docs/phase6.md): Velero with file-level volume backups (Kopia,
// encrypted with the recovery key) to an S3-compatible Object Storage
// bucket, configured in ConsoleSettings.spec.backups. A BackupPlan is a
// Velero Schedule; a Restore is a Velero Restore of one project.

// BackupScope is what a BackupPlan covers.
// +kubebuilder:validation:Enum=Cluster;Projects
type BackupScope string

const (
	// BackupCluster covers every project and Kwerft's own state (its
	// database, data key and settings): what install.sh --restore needs to
	// rebuild the console on a new server.
	BackupCluster BackupScope = "Cluster"
	// BackupProjects covers the listed projects only.
	BackupProjects BackupScope = "Projects"
)

// BackupPlanSpec schedules backups.
//
// +kubebuilder:validation:XValidation:rule="self.scope != 'Projects' || (has(self.projects) && size(self.projects) > 0)",message="a Projects plan lists at least one project"
type BackupPlanSpec struct {
	// +kubebuilder:default=Cluster
	// +optional
	Scope BackupScope `json:"scope,omitempty"`

	// Projects of a Projects plan.
	// +kubebuilder:validation:MaxItems=100
	// +listType=set
	// +optional
	Projects []string `json:"projects,omitempty"`

	// Schedule in cron syntax, in UTC.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=100
	Schedule string `json:"schedule"`

	// Retention: how long each backup is kept (default 14 days).
	// +optional
	Retention *metav1.Duration `json:"retention,omitempty"`

	// Volumes includes the data of the projects' Volumes and of App and
	// Task disks (file level). Off backs up the objects only.
	// +kubebuilder:default=true
	// +optional
	Volumes *bool `json:"volumes,omitempty"`

	// Paused stops scheduled runs; "Back up now" still works.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// BackupRun is one backup of a plan.
type BackupRun struct {
	// Name of the Velero Backup.
	Name string `json:"name"`
	// Phase: InProgress, Completed, PartiallyFailed, Failed.
	Phase string `json:"phase"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// +optional
	Items int32 `json:"items,omitempty"`
	// +optional
	Warnings int32 `json:"warnings,omitempty"`
	// +optional
	Errors int32 `json:"errors,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// BackupPlanStatus is written by the BackupPlan reconciler.
type BackupPlanStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	LastBackup *BackupRun `json:"lastBackup,omitempty"`
	// LastSuccessfulAt is when the newest Completed backup finished.
	// +optional
	LastSuccessfulAt *metav1.Time `json:"lastSuccessfulAt,omitempty"`
	// +optional
	NextRunAt *metav1.Time `json:"nextRunAt,omitempty"`
	// Backups is how many of this plan's backups the bucket holds.
	// +optional
	Backups int32 `json:"backups,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BackupPlan backs up the cluster or some projects on a schedule.
// Cluster-scoped; owners and admins manage plans.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Last",type=string,JSONPath=`.status.lastBackup.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BackupPlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupPlanSpec   `json:"spec"`
	Status BackupPlanStatus `json:"status,omitempty"`
}

// BackupPlanList contains a list of BackupPlans.
//
// +kubebuilder:object:root=true
type BackupPlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupPlan `json:"items"`
}

// RestoreSpec brings a project, or some of its Apps, back from a backup.
// A whole cluster is restored by install.sh --restore on a new server, not
// by a Restore.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a Restore cannot be changed; create a new one"
type RestoreSpec struct {
	// Backup is the Velero Backup's name.
	// +kubebuilder:validation:MinLength=1
	Backup string `json:"backup"`

	// Project to restore from the backup.
	// +kubebuilder:validation:MinLength=1
	Project string `json:"project"`

	// TargetProject restores under another name, next to the original
	// (a new project); empty: into the original project, replacing only
	// objects that no longer exist.
	// +optional
	TargetProject string `json:"targetProject,omitempty"`

	// Apps limits the restore to these Apps with their Volumes, SecretSets
	// and Domains; empty: the whole project.
	// +kubebuilder:validation:MaxItems=100
	// +listType=set
	// +optional
	Apps []string `json:"apps,omitempty"`
}

// RestoreStatus is written by the Restore reconciler.
type RestoreStatus struct {
	// Phase: Pending, InProgress, Completed, PartiallyFailed, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// VeleroRestore is the Velero Restore's name.
	// +optional
	VeleroRestore string `json:"veleroRestore,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	Warnings int32 `json:"warnings,omitempty"`
	// +optional
	Errors int32 `json:"errors,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Restore is one restore of a project from a backup. Cluster-scoped;
// owners and admins create them (annotation kwerft.dev/requested-by).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.spec.backup`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Restore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreSpec   `json:"spec"`
	Status RestoreStatus `json:"status,omitempty"`
}

// RestoreList contains a list of Restores.
//
// +kubebuilder:object:root=true
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Restore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupPlan{}, &BackupPlanList{}, &Restore{}, &RestoreList{})
}
