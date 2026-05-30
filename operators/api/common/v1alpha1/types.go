/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package v1alpha1 holds API types shared across the platform, agents, and
// governance groups. These are plain object fragments — not Kinds — so the
// package carries no group/version and registers nothing with a scheme.
// +kubebuilder:object:generate=true
package v1alpha1

// LocalRef references a CR by name in the same namespace.
type LocalRef struct {
	Name string `json:"name"`
}
