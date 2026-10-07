// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeSpec is a disk that Apps and Tasks in the same project mount by name
// (AppVolume.volume), e.g. a job writes a file that a server then reads.
//
// The disk is ReadWriteOnce: every pod that mounts it runs on the same node.
// local-nvme volumes are pinned to the node they were created on; for
// hcloud-volume, pods that share a Volume prefer the node it is attached to.
//
// +kubebuilder:validation:XValidation:rule="self.class == oldSelf.class",message="class cannot be changed; create a new Volume"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.size)).compareTo(quantity(string(oldSelf.size))) >= 0",message="size can only grow"
type VolumeSpec struct {
	// Size of the disk. Growing it works for hcloud-volume; local-nvme
	// volumes cannot be resized.
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).isGreaterThan(quantity('0'))",message="size must be positive"
	Size resource.Quantity `json:"size"`

	// Class is local-nvme (fastest, pinned to one node) or hcloud-volume
	// (movable between Hetzner Cloud nodes).
	// +kubebuilder:validation:Enum=local-nvme;hcloud-volume
	// +kubebuilder:default=local-nvme
	// +optional
	Class string `json:"class,omitempty"`
}

// VolumeStatus is written by the Volume reconciler.
type VolumeStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase of the claim: Pending (local-nvme binds when the first pod
	// mounts it), Bound or Lost.
	// +optional
	Phase corev1.PersistentVolumeClaimPhase `json:"phase,omitempty"`

	// Capacity actually provisioned; can be larger than spec.size.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`

	// ClaimName is the PersistentVolumeClaim behind this Volume.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// UsedBy lists the Apps, Schedules and unfinished Tasks that mount this
	// Volume, as "App/api" or "Task/import-1". While it is not empty, deleting
	// the Volume waits (the volume-protection finalizer).
	// +optional
	UsedBy []string `json:"usedBy,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Volume is a persistent disk in a project that Apps and Tasks share.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vol
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="name must be at most 63 characters"
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.class`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeSpec   `json:"spec"`
	Status VolumeStatus `json:"status,omitempty"`
}

// VolumeList contains a list of Volumes.
//
// +kubebuilder:object:root=true
type VolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Volume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Volume{}, &VolumeList{})
}
