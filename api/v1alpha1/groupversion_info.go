// Package v1alpha1 contains the Materialization API.
// +kubebuilder:object:generate=true
// +groupName=c4.avalanche.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version.
	GroupVersion = schema.GroupVersion{Group: "c4.avalanche.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
