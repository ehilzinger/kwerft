package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AppSource says where the container image comes from. Exactly one of Image
// or Git is set.
//
// +kubebuilder:validation:XValidation:rule="has(self.image) != has(self.git)",message="set exactly one of image or git"
type AppSource struct {
	// +optional
	Image *ImageSource `json:"image,omitempty"`
	// +optional
	Git *GitSource `json:"git,omitempty"`
}

// ImageSource deploys an existing image from any registry.
type ImageSource struct {
	// Ref is a full image reference, e.g. ghcr.io/acme/api:1.42.0.
	// +kubebuilder:validation:MinLength=1
	Ref string `json:"ref"`

	// PullSecret names a registry credential Secret in the app's namespace.
	// +optional
	PullSecret string `json:"pullSecret,omitempty"`
}

// GitSource builds the image in-cluster from a Git repository.
type GitSource struct {
	// Repository URL, e.g. https://github.com/acme/api.git or git@host:acme/api.git.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Branch to build and, with AutoDeploy, to follow.
	// +kubebuilder:default=main
	// +optional
	Branch string `json:"branch,omitempty"`

	// Path is the build context inside the repository.
	// +kubebuilder:default="/"
	// +optional
	Path string `json:"path,omitempty"`

	// Builder selects how the image is built.
	// +kubebuilder:validation:Enum=dockerfile;railpack
	// +kubebuilder:default=dockerfile
	// +optional
	Builder string `json:"builder,omitempty"`

	// Dockerfile path relative to Path, when Builder is dockerfile.
	// +kubebuilder:default=Dockerfile
	// +optional
	Dockerfile string `json:"dockerfile,omitempty"`

	// Connection names the GitConnection (GitHub App, GitLab, deploy key, ...)
	// that grants access to the repository. Empty for public repositories.
	// +optional
	Connection string `json:"connection,omitempty"`

	// AutoDeploy builds and deploys every push to Branch.
	// +kubebuilder:default=true
	// +optional
	AutoDeploy *bool `json:"autoDeploy,omitempty"`
}

// AppPort exposes a container port inside the cluster and optionally on a
// public hostname through the Gateway.
type AppPort struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Container int32 `json:"container"`

	// Public is a hostname served over HTTPS, e.g. api.example.com.
	// +optional
	Public string `json:"public,omitempty"`

	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// AppVolume mounts a persistent volume into every replica.
type AppVolume struct {
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	Size resource.Quantity `json:"size"`

	// Class is local-nvme (fastest, pinned to one node) or hcloud-volume
	// (movable between Hetzner Cloud nodes).
	// +kubebuilder:validation:Enum=local-nvme;hcloud-volume
	// +kubebuilder:default=local-nvme
	// +optional
	Class string `json:"class,omitempty"`
}

// HealthCheck is used for readiness and liveness.
type HealthCheck struct {
	// HTTP path; empty means a TCP check on Port.
	// +optional
	HTTP string `json:"http,omitempty"`
	Port int32  `json:"port"`
}

// AppSpec is everything the deploy wizard collects.
type AppSpec struct {
	Source AppSource `json:"source"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Size is a preset for requests and limits; custom uses Resources.
	// +kubebuilder:validation:Enum=small;medium;large;custom
	// +kubebuilder:default=small
	// +optional
	Size string `json:"size,omitempty"`

	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// +optional
	Command []string `json:"command,omitempty"`

	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// +optional
	Ports []AppPort `json:"ports,omitempty"`

	// AllowFrom lists apps ("api" or "project/api") that may connect to this
	// app. Everything else is denied. Gateway traffic to public ports is
	// always allowed.
	// +optional
	AllowFrom []string `json:"allowFrom,omitempty"`

	// Egress controls outbound traffic to the internet.
	// +kubebuilder:validation:Enum=none;https;all
	// +kubebuilder:default=https
	// +optional
	Egress string `json:"egress,omitempty"`

	// +optional
	Volumes []AppVolume `json:"volumes,omitempty"`

	// +optional
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`
}

// AppRevision records one rollout for history and rollback.
type AppRevision struct {
	Number int64  `json:"number"`
	Image  string `json:"image"`
	// Generation is the App spec generation this revision rolled out.
	Generation int64 `json:"generation"`
	// +optional
	Build string      `json:"build,omitempty"`
	Time  metav1.Time `json:"time"`
}

// AppStatus is written by the App reconciler.
type AppStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Image is the fully resolved image currently rolled out.
	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	Revision int64 `json:"revision,omitempty"`

	// History holds the most recent revisions, newest first.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	History []AppRevision `json:"history,omitempty"`

	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// +optional
	URLs []string `json:"urls,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// App is a deployable workload: an image or a Git repository plus how to run
// and expose it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyReplicas
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Revision",type=integer,JSONPath=`.status.revision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type App struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppSpec   `json:"spec,omitempty"`
	Status AppStatus `json:"status,omitempty"`
}

// AppList contains a list of Apps.
//
// +kubebuilder:object:root=true
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []App `json:"items"`
}

func init() {
	SchemeBuilder.Register(&App{}, &AppList{})
}
