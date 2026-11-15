// Package v1beta1 contains the input type for this Function
// +kubebuilder:object:generate=true
// +groupName=appdbschemas.fn.k8s-notes.example
// +versionName=v1beta1
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This isn't a custom resource, in the sense that we never install its CRD.
// It is a KRM-like object, so we generate a CRD to describe its schema.

// Input is the step input of function-appdb-schemas.
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:categories=crossplane
type Input struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// ProviderConfig names the provider-sql ProviderConfig in the XR's
	// namespace. Empty means the XR's own name.
	// +optional
	ProviderConfig string `json:"providerConfig,omitempty"`

	// Reserved lists more schema names to refuse, on top of public,
	// information_schema and anything starting pg_.
	// +optional
	Reserved []string `json:"reserved,omitempty"`

	// Break makes the function fail on purpose, for the book's break pages:
	// "panic" panics, "bare-response" answers without response.To.
	// +optional
	// +kubebuilder:validation:Enum=panic;bare-response
	Break string `json:"break,omitempty"`
}
