package v1alpha1

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReservedProjectNames are namespaces a Project must never take over, on top
// of every kube-* and kwerft-* name (the platform's own namespaces: system,
// builds, observability and any it adds later). Keep in sync with the
// XValidation rule on Project.
var ReservedProjectNames = []string{"default", "cert-manager", "traefik", "monitoring"}

// IsReservedProjectName reports whether name belongs to the platform and so
// cannot be a Project.
func IsReservedProjectName(name string) bool {
	return slices.Contains(ReservedProjectNames, name) || strings.HasPrefix(name, "kube-") || strings.HasPrefix(name, "kwerft-")
}

// ProjectSpec describes a group of apps that share a namespace, quotas and
// access rules.
type ProjectSpec struct {
	// DisplayName is shown in the console; the resource name is the namespace.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Quota caps the total resources the project may request.
	// Keys: requests.cpu, requests.memory, requests.storage, ...
	// +optional
	Quota corev1.ResourceList `json:"quota,omitempty"`

	// PodSecurity is the Pod Security Admission level enforced in the namespace.
	// +kubebuilder:validation:Enum=baseline;restricted
	// +kubebuilder:default=baseline
	// +optional
	PodSecurity string `json:"podSecurity,omitempty"`

	// Isolated denies traffic from other projects unless a TrafficRule allows it.
	// +kubebuilder:default=true
	// +optional
	Isolated *bool `json:"isolated,omitempty"`
}

// ProjectStatus is written by the Project reconciler.
type ProjectStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Project maps one-to-one to a namespace with the same name.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=proj
// +kubebuilder:validation:XValidation:rule="!(self.metadata.name in ['default', 'cert-manager', 'traefik', 'monitoring']) && !self.metadata.name.startsWith('kube-') && !self.metadata.name.startsWith('kwerft-')",message="this name is reserved for the platform"
// +kubebuilder:printcolumn:name="Display name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

// ProjectList contains a list of Projects.
//
// +kubebuilder:object:root=true
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
