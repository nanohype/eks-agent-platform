/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// ModelGatewaySpec configures a per-Platform gateway: the routes exposed,
// which Bedrock models back them, and which Guardrail attaches.
type ModelGatewaySpec struct {
	// PlatformRef is the owning Platform.
	PlatformRef commonv1alpha1.LocalRef `json:"platformRef"`

	// Routes is the list of named routes the gateway exposes.
	// +kubebuilder:validation:MinItems=1
	Routes []ModelRouteSpec `json:"routes"`

	// DefaultGuardrailRef applies when a Route does not specify its own.
	// +optional
	DefaultGuardrailRef *commonv1alpha1.LocalRef `json:"defaultGuardrailRef,omitempty"`
}

// ModelRouteSpec is a single named route.
type ModelRouteSpec struct {
	Name string `json:"name"`

	// ModelFamily: anthropic | meta | mistral | cohere | amazon-titan |
	// amazon-nova | stability.
	// +kubebuilder:validation:Enum=anthropic;meta;mistral;cohere;amazon-titan;amazon-nova;stability
	ModelFamily string `json:"modelFamily"`

	// ModelId is the canonical Bedrock model ID or inference profile ID.
	ModelID string `json:"modelId"`

	// CrossRegionProfile enables a Bedrock cross-region inference profile.
	// +optional
	CrossRegionProfile string `json:"crossRegionProfile,omitempty"`

	// RateLimit caps requests per minute (not tokens) on this route. The
	// operator renders it into an agentgateway local rate-limit policy with
	// unit=Minutes; 0 or unset disables rate limiting for the route.
	// +optional
	RateLimit int32 `json:"rateLimit,omitempty"`

	// GuardrailRef overrides the gateway's default guardrail.
	// +optional
	GuardrailRef *commonv1alpha1.LocalRef `json:"guardrailRef,omitempty"`
}

// ModelGatewayStatus surfaces the agentgateway Route/Listener state.
type ModelGatewayStatus struct {
	// Phase: Pending, Provisioning, Ready, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoint is the cluster-internal hostname of the gateway.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ObservedGeneration is the last spec.generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mgw
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platformRef.name`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// ModelGateway is a per-Platform gateway CR that fronts Bedrock for one or more named routes.
type ModelGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelGatewaySpec   `json:"spec,omitempty"`
	Status ModelGatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelGatewayList is the list-form of ModelGateway.
type ModelGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelGateway{}, &ModelGatewayList{})
}
