/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlatformSpec defines the desired state of a Platform — a tenancy boundary
// hosting one or more AgentFleets, with its own budget, identity, and
// guardrails.
type PlatformSpec struct {
	// DisplayName is a human-readable name for dashboards and CLI output.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Persona drives default values for AgentFleet, ModelGateway, and
	// dashboards. One of: sales-ops, support, finance, ops, founder, eng,
	// marketing, legal, generic.
	// +kubebuilder:validation:Enum=sales-ops;support;finance;ops;founder;eng;marketing;legal;generic
	// +kubebuilder:default=generic
	Persona string `json:"persona"`

	// Tenant is the owning Tenant CR (one Tenant can own multiple Platforms).
	Tenant string `json:"tenant"`

	// Budget references a BudgetPolicy CR in the same namespace.
	Budget BudgetRef `json:"budget"`

	// Identity controls how the IRSA role is named + which Bedrock models are
	// reachable.
	Identity IdentitySpec `json:"identity"`

	// Compliance flags drive stricter defaults across the Platform.
	// +optional
	Compliance ComplianceSpec `json:"compliance,omitempty"`

	// Isolation: namespace (default) or vCluster (hard isolation).
	// +kubebuilder:validation:Enum=namespace;vcluster
	// +kubebuilder:default=namespace
	// +optional
	Isolation string `json:"isolation,omitempty"`
}

// BudgetRef points at a BudgetPolicy by name.
type BudgetRef struct {
	Name string `json:"name"`
}

// IdentitySpec wires the per-Platform IRSA role.
// +kubebuilder:validation:XValidation:rule="!(has(self.allowedModels) && size(self.allowedModels) > 0 && has(self.allowedModelFamilies) && size(self.allowedModelFamilies) > 0)",message="allowedModels and allowedModelFamilies are mutually exclusive"
type IdentitySpec struct {
	// AllowedModels is the list of Bedrock model IDs (or inference-profile IDs)
	// the IRSA role can invoke. Mutually exclusive with AllowedModelFamilies.
	// +optional
	AllowedModels []string `json:"allowedModels,omitempty"`

	// AllowedModelFamilies (e.g. ["anthropic", "meta", "amazon-nova"]) is
	// expanded by the controller into ARNs at reconcile time.
	// +optional
	AllowedModelFamilies []string `json:"allowedModelFamilies,omitempty"`

	// ExtraPolicyArns are managed IAM policies attached on top of the baseline.
	// +optional
	ExtraPolicyArns []string `json:"extraPolicyArns,omitempty"`
}

// ComplianceSpec enables stricter defaults.
type ComplianceSpec struct {
	// HIPAA: object-lock compliance mode, no cross-region inference, PII detect
	// required on Guardrails.
	// +optional
	HIPAA bool `json:"hipaa,omitempty"`

	// SOC2: invocation logging required, kill-switch enabled.
	// +optional
	SOC2 bool `json:"soc2,omitempty"`
}

// PlatformStatus captures the controller's view of the world.
type PlatformStatus struct {
	// Phase: Pending, Provisioning, Ready, Suspended, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// IamRoleArn is the per-Platform IRSA role created by the controller.
	// +optional
	IamRoleArn string `json:"iamRoleArn,omitempty"`

	// Namespace is the tenant namespace the controller provisioned.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ObservedGeneration is the last spec.generation the controller reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SuspendedAt is the timestamp at which the kill-switch fired. When
	// non-nil the operator stops reattaching the baseline IAM policy and
	// the AgentFleetReconciler scales fleets to zero. Resets to nil only
	// when ops clears the iam:TagRole 'agents.stxkxs.io/suspended'
	// marker on the tenant IRSA role.
	// +optional
	SuspendedAt *metav1.Time `json:"suspendedAt,omitempty"`

	// SuspendedReason carries the kill-switch's reason (e.g.
	// 'budget-exceeded'). Same lifecycle as SuspendedAt.
	// +optional
	SuspendedReason string `json:"suspendedReason,omitempty"`

	// Conditions follows the standard kubernetes pattern.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=plat
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Persona",type=string,JSONPath=`.spec.persona`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Platform is the top-level tenancy CR. Namespaced so that BudgetPolicy,
// ModelGateway, AgentFleet, and EvalSuite references resolve in the same
// namespace by name. The operator provisions the tenant workload namespace
// (tenants-<platform-name>) separately at reconcile time; the Platform CR
// itself lives in whichever namespace the cluster admin places it (typically
// a management namespace such as eks-agent-platform).
type Platform struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformSpec   `json:"spec,omitempty"`
	Status PlatformStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlatformList is the list-form of Platform.
type PlatformList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Platform `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Platform{}, &PlatformList{})
}
