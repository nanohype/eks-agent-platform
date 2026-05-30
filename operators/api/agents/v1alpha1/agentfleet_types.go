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

// AgentFleetSpec composes kagent Agent / ModelConfig / ToolServer CRs plus
// platform-specific scaffolding (KEDA, NetworkPolicy, IRSA binding).
type AgentFleetSpec struct {
	PlatformRef commonv1alpha1.LocalRef `json:"platformRef"`

	// Agents is the list of agents to provision in this fleet.
	// +kubebuilder:validation:MinItems=1
	Agents []AgentSpec `json:"agents"`

	// Scaling controls KEDA's ScaledObject for the runtime Deployments.
	// +optional
	Scaling ScalingSpec `json:"scaling,omitempty"`

	// Compute optionally requests an AcceleratorClaim.
	// +optional
	Compute *ComputeSpec `json:"compute,omitempty"`
}

// AgentSpec is one agent in the fleet.
type AgentSpec struct {
	Name string `json:"name"`

	// SystemPrompt is the agent's instruction text.
	SystemPrompt string `json:"systemPrompt"`

	// ModelRoute is the named route on the Platform's ModelGateway.
	ModelRoute string `json:"modelRoute"`

	// Tools is the list of kagent ToolServer references.
	// +optional
	Tools []ToolRef `json:"tools,omitempty"`

	// Replicas overrides the fleet-wide scaling minimum for this agent.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// ToolRef references a kagent ToolServer by name.
type ToolRef struct {
	Name string `json:"name"`
}

// ScalingSpec configures KEDA.
type ScalingSpec struct {
	// Enabled — when false, the operator scales the Deployment to 0 and
	// removes the ScaledObject. Toggled false by the kill-switch on budget
	// breach.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// Min replicas. Use a pointer so 0 (kill-switch state) is distinguishable
	// from "field absent" — with int32 + omitempty, the zero value gets
	// dropped and re-defaulted, making min=0 unrepresentable.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Min *int32 `json:"min,omitempty"`

	// Max replicas.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +optional
	Max *int32 `json:"max,omitempty"`

	// QueueDepthTrigger: scale up when SQS depth exceeds this value.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +optional
	QueueDepthTrigger int32 `json:"queueDepthTrigger,omitempty"`

	// QueueUrl is the SQS queue the fleet's work originates from. When
	// set the operator emits a KEDA aws-sqs-queue trigger; otherwise a
	// CPU-utilization placeholder. The tenant IRSA role must have
	// sqs:GetQueueAttributes on this queue (granted via the agent-iam
	// baseline policy + an in-policy resource ARN derived from the URL).
	// +optional
	// +kubebuilder:validation:Pattern=`^https://sqs\.[a-z0-9-]+\.amazonaws\.com/[0-9]{12}/[A-Za-z0-9_-]+(\.fifo)?$`
	QueueURL string `json:"queueUrl,omitempty"`
}

// ComputeSpec requests accelerator resources via DRA.
type ComputeSpec struct {
	// AcceleratorClaim references an AcceleratorClaim CR. The operator
	// translates that into a ResourceClaimTemplate referenced in the pod spec.
	AcceleratorClaim commonv1alpha1.LocalRef `json:"acceleratorClaim"`

	// Resources are pod resource requests/limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// AgentFleetStatus reports rollout state.
type AgentFleetStatus struct {
	// Phase: Pending, Provisioning, Ready, ScaledToZero, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ReadyAgents counts agents whose downstream Deployment is ready.
	// +optional
	ReadyAgents int32 `json:"readyAgents,omitempty"`

	// ObservedGeneration is the last spec.generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=fleet
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platformRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyAgents`

// AgentFleet is a Platform-scoped composition of one or more agents on top
// of upstream kagent CRs. The scale subresource is deliberately omitted:
// `kubectl scale` would be ambiguous (min? max? per-agent?) for a fleet,
// so per-agent replica overrides live on AgentSpec.Replicas and fleet-wide
// behavior is driven by .spec.scaling (KEDA) instead.
type AgentFleet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentFleetSpec   `json:"spec,omitempty"`
	Status AgentFleetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentFleetList is the list-form of AgentFleet.
type AgentFleetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentFleet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentFleet{}, &AgentFleetList{})
}
