package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildPhase is the lifecycle of a Build.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Cancelled
type BuildPhase string

const (
	BuildPending   BuildPhase = "Pending"
	BuildRunning   BuildPhase = "Running"
	BuildSucceeded BuildPhase = "Succeeded"
	BuildFailed    BuildPhase = "Failed"
	BuildCancelled BuildPhase = "Cancelled"
)

// BuildSource is the App's Git source as it was when the build was created,
// so a build is reproducible and its log matches what was built even if the
// App's settings change meanwhile.
type BuildSource struct {
	// Repository URL, as in GitSource.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`
	// +optional
	Path string `json:"path,omitempty"`
	// +kubebuilder:validation:Enum=dockerfile;railpack
	Builder string `json:"builder"`
	// +optional
	Dockerfile string `json:"dockerfile,omitempty"`
	// +optional
	Connection string `json:"connection,omitempty"`
}

// BuildSpec pins one commit of an App's Git source. Builds are immutable:
// rebuilding creates a new Build. Create them with internal/builds.New.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="builds are immutable"
type BuildSpec struct {
	// App is the name of the App in the same namespace.
	// +kubebuilder:validation:MinLength=1
	App string `json:"app"`

	// Source is a snapshot of the App's Git source.
	Source BuildSource `json:"source"`

	// Commit is the full SHA to build.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit"`

	// +optional
	Branch string `json:"branch,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	Author string `json:"author,omitempty"`

	// Trigger records why the build started.
	// +kubebuilder:validation:Enum=push;manual;pull-request
	Trigger string `json:"trigger"`

	// RequestedBy is the console user ("manual") or the Git account that
	// pushed ("push", "pull-request").
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`

	// PullRequest number, for pull-request builds.
	// +optional
	PullRequest int64 `json:"pullRequest,omitempty"`

	// Deploy rolls the App out to the built image on success. False for
	// pull-request builds, which only report a check.
	// +optional
	Deploy bool `json:"deploy,omitempty"`
}

// BuildStatus is written by the Build reconciler.
type BuildStatus struct {
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`

	// Number counts the App's builds: #1, #2, ... Assigned by the reconciler
	// when it first sees the build.
	// +optional
	Number int64 `json:"number,omitempty"`

	// Image is the pushed reference, e.g. registry.kwerft.internal/storefront/api:4f2c1ab.
	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Digest of the pushed image (sha256:...).
	// +optional
	Digest string `json:"digest,omitempty"`

	// Job is the name of the Kubernetes Job running BuildKit, in the
	// kwerft-builds namespace.
	// +optional
	Job string `json:"job,omitempty"`

	// Pod is the Job's pod, whose logs are the build log.
	// +optional
	Pod string `json:"pod,omitempty"`

	// Message says why a build failed or is waiting (queued behind others).
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Build is one in-cluster image build of an App with a Git source.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.app`
// +kubebuilder:printcolumn:name="Number",type=integer,JSONPath=`.status.number`
// +kubebuilder:printcolumn:name="Commit",type=string,JSONPath=`.spec.commit`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Build struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BuildSpec   `json:"spec,omitempty"`
	Status BuildStatus `json:"status,omitempty"`
}

// BuildList contains a list of Builds.
//
// +kubebuilder:object:root=true
type BuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Build `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Build{}, &BuildList{})
}
