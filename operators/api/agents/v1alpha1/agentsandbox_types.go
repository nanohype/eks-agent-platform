/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// AgentSandboxSpec declares one ephemeral, hardened pod that runs a single
// agent role-session — fab's `sdk` role-loop dispatched per session. The
// reconciler builds the pod on the dedicated, tainted sandbox node pool,
// locked down by a default-deny NetworkPolicy, under the Platform's tenant
// ServiceAccount — which carries the tenant's AWS identity through its EKS
// Pod Identity association.
type AgentSandboxSpec struct {
	// PlatformRef is the owning Platform. The session pod runs in that
	// Platform's tenant namespace and the sandbox gates on Platform
	// readiness.
	PlatformRef commonv1alpha1.LocalRef `json:"platformRef"`

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

	// WritablePaths are additional absolute paths the session container may
	// write, mounted as emptyDir alongside /workspace and /tmp.
	//
	// The session runs with a read-only root filesystem, and the only paths this
	// operator can name for an arbitrary image are the two it already mounts:
	// /workspace, which is this CRD's own contract, and /tmp, which every
	// runtime expects. Everything else is a fact about the image — where its
	// user's HOME is, where its toolchain caches — and the image is yours.
	//
	// Declare what your entrypoint writes. Getting this wrong surfaces as the
	// container failing on a read-only filesystem at the moment it tries, which
	// is inside a tool call rather than at startup, so it is worth checking
	// against the image rather than discovering.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^/[A-Za-z0-9._/-]*$`
	// +optional
	WritablePaths []string `json:"writablePaths,omitempty"`

	// ActiveDeadlineSeconds is the wall-clock ceiling on one session, measured
	// from the moment the pod starts.
	//
	// It is what makes TTLSecondsAfterFinished reachable. That TTL counts from a
	// TERMINAL phase, so it collects a session that ended and says nothing about
	// one that never does: a hung agent — a tool call waiting on a socket
	// nothing will answer, a model call with no deadline of its own — leaves a
	// pod holding its node slot and its tenant credentials indefinitely, polled
	// every reconcile forever, with the garbage collector waiting on a phase
	// that will not arrive.
	//
	// Kubernetes enforces this one itself and marks the pod Failed on expiry,
	// which IS the terminal phase, so the existing TTL then collects it. The two
	// fields are one mechanism: this bounds the session, that bounds the corpse.
	//
	// Default 4h — comfortably past any legitimate agent session and far short
	// of a pod nobody notices for a week. Set 0 to disable, which is a decision
	// about a specific workload rather than the shape a sandbox ships with.
	// +kubebuilder:default=14400
	// +kubebuilder:validation:Minimum=0
	// +optional
	ActiveDeadlineSeconds *int32 `json:"activeDeadlineSeconds,omitempty"`
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
