package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskPhase is the lifecycle of a Task.
type TaskPhase string

const (
	TaskPending   TaskPhase = "Pending"
	TaskRunning   TaskPhase = "Running"
	TaskSucceeded TaskPhase = "Succeeded"
	TaskFailed    TaskPhase = "Failed"
)

// Finished reports whether the phase is final.
func (p TaskPhase) Finished() bool { return p == TaskSucceeded || p == TaskFailed }

// TaskSpec is one run of a container to completion. It has the shape of an
// App (source, command, env, volumes, resources, egress) minus ports,
// replicas and health checks.
//
// With FromApp the Task starts from that App: its image (also the built image
// of a Git App), command, env, size/resources, egress and shared Volumes.
// Fields set on the Task win: source, command, size/resources and egress
// replace the App's; env and volumes are merged by name and path. The App is
// read once, when the Task starts; later changes to it do not affect the run.
//
// Env applies in this order, later wins: the App's env, Env, EnvOverrides.
// EnvOverrides holds the per-run changes of "run now" (FORCE=1, DRY_RUN=1),
// kept apart from Env so a run shows what was different about it.
//
// +kubebuilder:validation:XValidation:rule="has(self.fromApp) || has(self.source)",message="set source or fromApp"
// +kubebuilder:validation:XValidation:rule="!has(self.source) || !has(self.source.git)",message="a Task runs an image; use fromApp to run the image built for a Git App"
type TaskSpec struct {
	// FromApp names an App in the same project to inherit from.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	FromApp string `json:"fromApp,omitempty"`

	// +optional
	Source *AppSource `json:"source,omitempty"`

	// Size is a preset for requests and limits; custom uses Resources.
	// Empty means the App's size with FromApp, else small.
	// +kubebuilder:validation:Enum=small;medium;large;custom
	// +optional
	Size string `json:"size,omitempty"`

	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Command replaces the image's entrypoint, one argument per item:
	// ["sh", "-c", "echo hi"], not ["sh -c echo hi"].
	// +kubebuilder:validation:XValidation:rule="size(self) == 0 || !self[0].matches('[[:space:]]')",message="the program (first item) contains whitespace; give each argument its own item, e.g. [\"echo\", \"hi\"], or use [\"sh\", \"-c\", \"...\"]"
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Command []string `json:"command,omitempty"`

	// +kubebuilder:validation:MaxItems=256
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvOverrides is applied last; set by "run now".
	// +kubebuilder:validation:MaxItems=64
	// +optional
	EnvOverrides []corev1.EnvVar `json:"envOverrides,omitempty"`

	// Egress controls outbound traffic to the internet. Empty means the
	// App's egress with FromApp, else https.
	// +kubebuilder:validation:Enum=none;https;all
	// +optional
	Egress string `json:"egress,omitempty"`

	// Volumes mounts shared Volumes. A Task has no disk of its own.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(v, has(v.volume))",message="a Task can only mount shared Volumes (volume), not a disk of its own (size)"
	// +optional
	Volumes []AppVolume `json:"volumes,omitempty"`

	// Timeout stops the run (all retries together) after this long, e.g. 30m.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="timeout must be at least 1s"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Retries is how often a failed run is started again.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=0
	// +optional
	Retries *int32 `json:"retries,omitempty"`

	// TTLSecondsAfterFinished is how long a finished run (its pod and logs)
	// is kept. Default 7 days.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=604800
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// +optional
	OnSuccess *TaskOnSuccess `json:"onSuccess,omitempty"`
}

// TaskOnSuccess lists what Kwerft does once the Task succeeds.
type TaskOnSuccess struct {
	// Restart rolls out these Apps in the same project, e.g. a server that has
	// to reopen a file the job replaced. Kwerft does it, not the job, so the
	// job needs no permissions of its own.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +optional
	Restart []string `json:"restart,omitempty"`
}

// TaskStatus is written by the Task reconciler.
type TaskStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	// +optional
	Phase TaskPhase `json:"phase,omitempty"`

	// Image is the image the run uses, resolved from the source or FromApp.
	// +optional
	Image string `json:"image,omitempty"`

	// Job is the Kubernetes Job running the Task.
	// +optional
	Job string `json:"job,omitempty"`

	// Pod is the most recent pod of the run, for its logs.
	// +optional
	Pod string `json:"pod,omitempty"`

	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// ExitCode of the container in the most recent pod that terminated.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// Conditions: Ready (True once succeeded; the reason says why not) and
	// AppsRestarted (onSuccess.restart was carried out).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Task is a one-off run of a container to completion: a migration, an
// import, a backup. Schedules create one Task per run, so scheduled and
// manual runs look the same. The spec cannot be changed; run again by
// creating a new Task.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="name must be at most 63 characters"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Exit",type=integer,JSONPath=`.status.exitCode`
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=`.status.startTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a Task cannot be changed; create a new one"
	Spec   TaskSpec   `json:"spec"`
	Status TaskStatus `json:"status,omitempty"`
}

// TaskList contains a list of Tasks.
//
// +kubebuilder:object:root=true
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
