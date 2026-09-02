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

// RouteAPI is the client-facing wire format a caller speaks to a route.
//
// It is not the same question as which upstream schema serves the route. A
// caller reaches every route through the same gateway, but the gateway serves
// each wire format under its own endpoint prefix, so the format decides the
// URL the caller must use — and which model families it can reach at all.
//
//   - Anthropic: native Anthropic Messages, at `<endpoint>/anthropic`. Keeps
//     the model's own shape end to end — thinking blocks, cache points, and
//     tool use survive. Anthropic-family foundation routes only.
//   - OpenAI: OpenAI chat completions and embeddings, at `<endpoint>/v1`. The
//     gateway translates to Bedrock. Reaches every family, which is what makes
//     a route repointable to a non-Anthropic model without touching the app.
//
// +kubebuilder:validation:Enum=Anthropic;OpenAI
type RouteAPI string

const (
	// RouteAPIAnthropic is the native Anthropic Messages wire format.
	RouteAPIAnthropic RouteAPI = "Anthropic"
	// RouteAPIOpenAI is the OpenAI chat-completions / embeddings wire format.
	RouteAPIOpenAI RouteAPI = "OpenAI"
)

// ModelRouteSpec is a single named route.
//
// +kubebuilder:validation:XValidation:rule="self.modelSource != 'foundation' || has(self.modelFamily)",message="modelFamily is required for a foundation route"
// +kubebuilder:validation:XValidation:rule="!has(self.api) || self.api != 'Anthropic' || (self.modelSource == 'foundation' && has(self.modelFamily) && self.modelFamily == 'anthropic')",message="api Anthropic is only available on an anthropic-family foundation route; every other model reaches callers as OpenAI"
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

	// API is the wire format callers speak to this route, and therefore which
	// base URL they must use — the gateway serves each format under its own
	// endpoint prefix. The reconciler publishes the resolved value and its base
	// URL on status.routes, so a caller reads the contract rather than assuming
	// it.
	//
	// Left unset it is derived from the model: an anthropic-family foundation
	// route serves Anthropic, everything else serves OpenAI. There is no static
	// default, because one would be wrong for whichever kind of route it did
	// not describe — an embeddings route is not reachable as Anthropic, and
	// defaulting a Claude route to OpenAI would silently drop thinking blocks
	// and cache points.
	//
	// Set it explicitly to pin the format across a model change: a route
	// declared OpenAI stays OpenAI when repointed from Claude to an
	// open-weight model, so the swap is a CR edit and the app is untouched.
	// +optional
	API RouteAPI `json:"api,omitempty"`

	// RateLimit caps requests per minute (not tokens) on this route. The
	// operator renders it into a local rate-limit rule on the gateway's
	// BackendTrafficPolicy; 0 or unset disables rate limiting for the route.
	// +optional
	RateLimit int32 `json:"rateLimit,omitempty"`

	// GuardrailRef overrides the gateway's default guardrail. On a foundation
	// route the guardrail attaches as request headers the caller cannot
	// override. On an imported
	// route an inline guardrail is not applicable (Bedrock inline guardrails are
	// foundation-model-only), so the route is served without one and the gateway
	// surfaces an ImportedRouteGuardrailUnenforced condition — enforcement via
	// ApplyGuardrail is a tracked follow-up.
	// +optional
	GuardrailRef *commonv1alpha1.LocalRef `json:"guardrailRef,omitempty"`
}

// RouteStatus is the published client contract for one route: what to call it
// with, and where.
type RouteStatus struct {
	// Name is the route name callers send as the model field.
	Name string `json:"name"`

	// API is the resolved wire format — spec.routes[].api when set, otherwise
	// derived from the model family.
	API RouteAPI `json:"api"`

	// BaseURL is the base a client of that wire format is configured with. It
	// is not status.endpoint: the gateway serves each format under its own
	// prefix, so the endpoint alone is not a usable base for any client. An
	// Anthropic SDK appends /v1/messages to this, an OpenAI one appends
	// /chat/completions or /embeddings.
	BaseURL string `json:"baseURL"`
}

// ModelGatewayStatus surfaces the gateway's route and listener state.
type ModelGatewayStatus struct {
	// Phase: Pending, Provisioning, Ready, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoint is the cluster-internal hostname of the gateway. It addresses
	// the gateway, not any one API on it — see Routes for the base URL a
	// client is actually configured with.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Routes is the per-route client contract: the resolved wire format and
	// the base URL that format is served at.
	// +optional
	// +listType=map
	// +listMapKey=name
	Routes []RouteStatus `json:"routes,omitempty"`

	// ObservedGeneration is the last spec.generation reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carry what the last reconcile learned. Two are written:
	//
	//	RoutesReconciled                  the routes were emitted (False while
	//	                                  waiting on the Platform or the
	//	                                  Gateway-API CRDs)
	//	ImportedRouteGuardrailUnenforced  whether any imported route is served
	//	                                  without its guardrail
	//
	// The second is three-valued, and the three are three different sentences.
	// True names the routes. False says the routes were walked and none of them
	// is unguarded. Unknown says the walk did not happen — which is every pass
	// that returns before the routes are reached — so a reader deciding whether
	// to point sensitive traffic at an imported route must treat Unknown as an
	// unanswered question rather than as an absence of findings.
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
