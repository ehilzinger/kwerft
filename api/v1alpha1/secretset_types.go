// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretSetSpec is a named group of secret values in a project (docs/plan.md
// › Secrets). The values live in the Secret of the same name, labelled
// kwerft.dev/secret-set, which the API writes as the user; the spec holds
// no values.
type SecretSetSpec struct {
	// +kubebuilder:validation:MaxLength=200
	// +optional
	Description string `json:"description,omitempty"`

	// Generate lists keys the reconciler fills with 32 random bytes
	// (base64url) while they are missing, e.g. a database password from a
	// template. Removing a key here keeps its value.
	// +kubebuilder:validation:MaxItems=50
	// +listType=set
	// +optional
	Generate []string `json:"generate,omitempty"`

	// Derived keys are computed from other keys of the set by the
	// reconciler, e.g. DATABASE_URL from PASSWORD.
	// +kubebuilder:validation:MaxItems=50
	// +listType=map
	// +listMapKey=key
	// +optional
	Derived []DerivedSecretKey `json:"derived,omitempty"`
}

// DerivedSecretKey is a value built from a template with ${KEY} references
// to the set's other keys, e.g.
// postgres://app:${PASSWORD}@postgres-main:5432/app.
type DerivedSecretKey struct {
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]{1,253}$`
	Key string `json:"key"`
	// +kubebuilder:validation:MaxLength=2048
	Template string `json:"template"`
}

// SecretKeyStatus describes one key, never its value.
type SecretKeyStatus struct {
	Name string `json:"name"`
	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
	// UpdatedBy is the console user who last set it; empty for generated
	// and derived keys.
	// +optional
	UpdatedBy string `json:"updatedBy,omitempty"`
	// Source: Set, Generated or Derived.
	// +optional
	Source string `json:"source,omitempty"`
}

// SecretSetStatus is written by the SecretSet reconciler with the
// controller's own permissions, so developers and viewers learn key names
// without reading the Secret.
type SecretSetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Keys []SecretKeyStatus `json:"keys,omitempty"`
	// UsedBy lists the Apps, Tasks and Schedules that reference this set,
	// as "App/api".
	// +optional
	UsedBy []string `json:"usedBy,omitempty"`
	// Missing lists references to keys the set does not have, as
	// "App/api: SMTP_PASSWORD".
	// +optional
	Missing []string `json:"missing,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SecretSet holds a project's secret values, shared by its Apps, Tasks and
// Schedules, or one App's own (<app>-env, owned by the App).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sset
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="name must be at most 63 characters"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SecretSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SecretSetSpec   `json:"spec,omitempty"`
	Status SecretSetStatus `json:"status,omitempty"`
}

// SecretSetList contains a list of SecretSets.
//
// +kubebuilder:object:root=true
type SecretSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecretSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SecretSet{}, &SecretSetList{})
}
