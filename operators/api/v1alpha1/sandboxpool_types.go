/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxPoolSpec declares a pool of Managed Agents self-hosted sandbox
// workers for a `self_hosted` environment. The workers run Anthropic's
// `ant beta:worker`, claiming sessions from the environment's work queue
// and executing agent tool calls inside the cluster.
type SandboxPoolSpec struct {
	// PlatformRef is the owning Platform. The pool's workers run in that
	// Platform's tenant namespace and the pool gates on Platform readiness.
	PlatformRef LocalRef `json:"platformRef"`

	// EnvironmentID is the Managed Agents self_hosted environment whose
	// work queue these workers drain (an `env_...` id).
	EnvironmentID string `json:"environmentId"`

	// EnvironmentKeySecret holds ANTHROPIC_ENVIRONMENT_KEY — the worker's
	// auth token, mounted into every worker pod.
	EnvironmentKeySecret corev1.SecretKeySelector `json:"environmentKeySecret"`

	// APIKeySecret holds the organization API key. It is consumed only by
	// the work-queue autoscaler, never mounted into worker pods — Anthropic
	// warns the org key must not be reachable by agent tool calls.
	// +optional
	APIKeySecret *corev1.SecretKeySelector `json:"apiKeySecret,omitempty"`

	// Image overrides the sandbox worker image. Defaults to the platform's
	// published sandbox-worker image when empty.
	// +optional
	Image string `json:"image,omitempty"`

	// Scaling bounds the worker count.
	// +optional
	Scaling SandboxScalingSpec `json:"scaling,omitempty"`

	// Resources are the per-worker-pod resource requests and limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// SandboxScalingSpec bounds the worker Deployment's replica count.
type SandboxScalingSpec struct {
	// MinReplicas is the worker-count floor. A pointer so 0 (scale to zero,
	// for the autoscaled path) is distinguishable from "field absent".
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the worker-count ceiling for the autoscaler.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// QueueDepthTarget is the work-queue depth per worker the autoscaler
	// aims for before adding workers.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	QueueDepthTarget int32 `json:"queueDepthTarget,omitempty"`
}

// SandboxPoolStatus reports the pool's reconciled state.
type SandboxPoolStatus struct {
	// Phase: Pending, Ready, Suspended, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ReadyWorkers is the worker Deployment's ready replica count.
	// +optional
	ReadyWorkers int32 `json:"readyWorkers,omitempty"`

	// ObservedGeneration is the last spec.generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=sbxpool
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platformRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyWorkers`

// SandboxPool is a Platform-scoped pool of Managed Agents self-hosted
// sandbox workers. The reconciler runs them as a Deployment on the
// dedicated, tainted sandbox node pool, locked down by a default-deny
// NetworkPolicy.
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxPoolSpec   `json:"spec,omitempty"`
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxPoolList is the list-form of SandboxPool.
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
