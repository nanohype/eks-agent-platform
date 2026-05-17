/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=agents.stxkxs.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion identifies the API group + version for this package.
	GroupVersion = schema.GroupVersion{Group: "agents.stxkxs.io", Version: "v1alpha1"}
	// SchemeBuilder registers the package's types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // kubebuilder convention
	// AddToScheme adds the registered types to a Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
