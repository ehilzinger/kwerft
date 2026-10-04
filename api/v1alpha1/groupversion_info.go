// Package v1alpha1 contains the werft.dev/v1alpha1 API: the resources the
// console reads and writes. Controllers render them into native objects.
//
// +kubebuilder:object:generate=true
// +groupName=werft.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version for all Werft resources.
	GroupVersion = schema.GroupVersion{Group: "werft.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
