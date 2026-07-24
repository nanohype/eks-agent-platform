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

// ModelSource discriminates how a route sources its model — the same
// create|adopt idiom the rest of the platform uses: a stable route interface
// either way, with the source-specific fields validated at the CRD boundary
// rather than silently ignored.
//   - foundation: a Bedrock foundation model or inference profile. modelFamily
//     is required; crossRegionProfile is available.
//   - imported: an open-weight model brought in through Bedrock Custom Model
//     Import. modelId is the imported-model ARN; modelFamily and
//     crossRegionProfile do not apply and are rejected.
//
// +kubebuilder:validation:Enum=foundation;imported
type ModelSource string

const (
	// ModelSourceFoundation routes to a Bedrock foundation model / inference profile.
	ModelSourceFoundation ModelSource = "foundation"
	// ModelSourceImported routes to a Custom Model Import open-weight model by ARN.
	ModelSourceImported ModelSource = "imported"
)

// ModelRouteSpec is a single named route.
//
// +kubebuilder:validation:XValidation:rule="self.modelSource != 'foundation' || has(self.modelFamily)",message="modelFamily is required for a foundation route"
// +kubebuilder:validation:XValidation:rule="self.modelSource != 'imported' || !has(self.modelFamily)",message="modelFamily does not apply to an imported route and must be omitted"
// +kubebuilder:validation:XValidation:rule="self.modelSource != 'imported' || !has(self.crossRegionProfile)",message="crossRegionProfile does not apply to an imported route and must be omitted"
// +kubebuilder:validation:XValidation:rule="self.modelSource != 'imported' || self.modelId.startsWith('arn:')",message="an imported route's modelId must be the imported-model ARN"
type ModelRouteSpec struct {
	Name string `json:"name"`

	// ModelSource discriminates a foundation-model route from an imported
	// (Custom Model Import) route. Defaults to foundation, so an existing
	// route that omits it stays a foundation route.
	// +kubebuilder:default=foundation
	// +optional
	ModelSource ModelSource `json:"modelSource,omitempty"`

	// ModelFamily is the Bedrock model family for a foundation route:
	// anthropic | meta | mistral | cohere | amazon-titan | amazon-nova |
	// stability. Required for a foundation route, rejected for an imported one
	// (enforced by the route-level CEL rules above).
	// +kubebuilder:validation:Enum=anthropic;meta;mistral;cohere;amazon-titan;amazon-nova;stability
	// +optional
	ModelFamily string `json:"modelFamily,omitempty"`

	// ModelID is the route's model. For a foundation route it is the canonical
	// Bedrock model ID or inference-profile ID; for an imported route it is the
	// imported-model ARN
	// (arn:<partition>:bedrock:<region>:<account>:imported-model/<id>).
	ModelID string `json:"modelId"`

	// CrossRegionProfile enables a Bedrock cross-region inference profile.
	// Foundation routes only; rejected on an imported route.
	// +optional
	CrossRegionProfile string `json:"crossRegionProfile,omitempty"`

	// RateLimit caps requests per minute (not tokens) on this route. The
	// operator renders it into an agentgateway local rate-limit policy with
	// unit=Minutes; 0 or unset disables rate limiting for the route.
	// +optional
	RateLimit int32 `json:"rateLimit,omitempty"`

	// GuardrailRef overrides the gateway's default guardrail. On a foundation
	// route the guardrail attaches inline to the Bedrock backend. On an imported
	// route an inline guardrail is not applicable (Bedrock inline guardrails are
	// foundation-model-only), so the route is served without one and the gateway
	// surfaces an ImportedRouteGuardrailUnenforced condition — enforcement via
	// ApplyGuardrail is a tracked follow-up.
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
