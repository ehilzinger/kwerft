package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DomainSpec is a public hostname served over HTTPS through the shared
// Gateway. Apps create a Domain for each public port automatically; the
// Domain reconciler gives it a Gateway listener and a certificate.
type DomainSpec struct {
	// Hostname, lowercase, e.g. api.example.com. Wildcards come with DNS-01.
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=253
	Hostname string `json:"hostname"`
}

// DomainStatus is written by the Domain reconciler.
type DomainStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Listener is the Gateway listener serving this hostname over HTTPS.
	// HTTPRoutes attach to it with sectionName.
	// +optional
	Listener string `json:"listener,omitempty"`

	// NotAfter is when the current certificate expires.
	// +optional
	NotAfter *metav1.Time `json:"notAfter,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Domain claims a hostname for the project (namespace) it lives in. When two
// projects claim the same hostname, the older Domain wins.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dom
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.status.notAfter`
type Domain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DomainSpec   `json:"spec,omitempty"`
	Status DomainStatus `json:"status,omitempty"`
}

// DomainList contains a list of Domains.
//
// +kubebuilder:object:root=true
type DomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Domain `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Domain{}, &DomainList{})
}
