/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSandboxSpec declares one ephemeral, hardened pod that runs a single
// agent role-session — fab's `sdk` role-loop dispatched per session. The
// reconciler builds the pod on the dedicated, tainted sandbox node pool,
// locked down by a default-deny NetworkPolicy, under the Platform's tenant
// IRSA ServiceAccount.
type AgentSandboxSpec struct {
	// PlatformRef is the owning Platform. The session pod runs in that
	// Platform's tenant namespace and the sandbox gates on Platform
	// readiness.
	PlatformRef LocalRef `json:"platformRef"`

	// Image is the container image the session pod runs.
	Image string `json:"image"`

	// Command overrides the image entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args are the container arguments.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is the session pod's environment. The dispatcher (fab) passes the
	// role, the role message, and any backend config through here.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// RuntimeClassName selects a Kubernetes RuntimeClass for the session
	// pod — "gvisor" or "kata" for kernel-level isolation of the untrusted
	// agent code. The named RuntimeClass must already exist. Empty uses the
	// cluster's default runtime.
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// Resources are the session pod's resource requests and limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// TTLSecondsAfterFinished is how long the AgentSandbox is kept after its
	// session pod terminates before the operator garbage-collects it.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// AgentSandboxStatus reports the sandbox's reconciled state.
type AgentSandboxStatus struct {
	// Phase: Pending, Running, Succeeded, Failed, Suspended.
	// +optional
	Phase string `json:"phase,omitempty"`

	// PodName is the session pod's name in the tenant namespace.
	// +optional
	PodName string `json:"podName,omitempty"`

	// PodPhase mirrors the session pod's status.phase.
	// +optional
	PodPhase string `json:"podPhase,omitempty"`

	// CompletedAt is when the session pod first reached a terminal phase —
	// the start of the TTL countdown.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the last spec.generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=agsbx
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platformRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`

// AgentSandbox is a Platform-scoped, single-use isolated pod for one agent
// role-session. It shares SandboxPool's hardening — Pod Security
// "restricted", default-deny networked, on the dedicated tainted node pool —
// but is push-dispatched (one session, run-once) rather than a pull-based
// pool of always-on workers.
type AgentSandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSandboxSpec   `json:"spec,omitempty"`
	Status AgentSandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentSandboxList is the list-form of AgentSandbox.
type AgentSandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentSandbox{}, &AgentSandboxList{})
}
