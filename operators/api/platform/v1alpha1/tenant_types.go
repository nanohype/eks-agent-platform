/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantSpec describes an organization (or sub-org) that owns one or more
// Platforms. Tenant is cluster-scoped — it doesn't represent a Kubernetes
// namespace; it represents an organizational boundary that crosses
// Platforms. The relationship to Platform is by `Platform.spec.tenant`
// referencing `Tenant.metadata.name`.
type TenantSpec struct {
	// DisplayName is the human-readable tenant name shown in dashboards
	// and persona UX.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// PrimaryPersona drives default values for new Platforms onboarded
	// into this tenant. One of the standard persona names.
	// +kubebuilder:validation:Enum=sales-ops;support;finance;ops;founder;eng;marketing;legal;generic
	// +kubebuilder:default=generic
	PrimaryPersona string `json:"primaryPersona"`

	// Contact carries human-readable owner info (Slack channel, on-call
	// rotation, billing email) for ops to reach.
	// +optional
	Contact ContactSpec `json:"contact,omitempty"`

	// Compliance baseline applied to every Platform owned by this Tenant
	// unless the Platform itself sets a stricter value.
	// +optional
	Compliance ComplianceSpec `json:"compliance,omitempty"`

	// AggregateMonthlyBudgetUsd is the soft cap on the SUM of all owned
	// Platforms' BudgetPolicy.spec.monthlyUsd. Status reports whether the
	// sum exceeds this; the operator does not enforce — each Platform's
	// own BudgetPolicy is the enforcement layer. Modeled as a decimal-
	// string to mirror BudgetPolicy.monthlyUsd.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,2})?$`
	// +optional
	AggregateMonthlyBudgetUsd string `json:"aggregateMonthlyBudgetUsd,omitempty"`
}

// ContactSpec carries owner / on-call / billing reach paths.
type ContactSpec struct {
	// SlackChannel for tenant-wide notifications (e.g. "#acme-ops").
	// +optional
	SlackChannel string `json:"slackChannel,omitempty"`

	// OncallRotation — Pagerduty schedule key or similar identifier.
	// +optional
	OncallRotation string `json:"oncallRotation,omitempty"`

	// BillingEmail — invoice + budget-breach notification recipient.
	// +optional
	BillingEmail string `json:"billingEmail,omitempty"`
}

// TenantStatus aggregates the state of Platforms owned by this Tenant.
type TenantStatus struct {
	// Phase: Pending, Active, Suspended (any owned Platform suspended),
	// Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// PlatformCount is the number of Platform CRs whose
	// spec.tenant == Tenant.metadata.name.
	// +optional
	PlatformCount int32 `json:"platformCount,omitempty"`

	// ReadyPlatformCount is the subset of PlatformCount in phase=Ready.
	// +optional
	ReadyPlatformCount int32 `json:"readyPlatformCount,omitempty"`

	// SuspendedPlatformCount is the subset in phase=Suspended.
	// +optional
	SuspendedPlatformCount int32 `json:"suspendedPlatformCount,omitempty"`

	// AggregateSpendUsd is the sum of CurrentSpendUsd across all owned
	// BudgetPolicies (one per owned Platform).
	// +optional
	AggregateSpendUsd string `json:"aggregateSpendUsd,omitempty"`

	// AggregateBudgetUsd is the sum of MonthlyUsd across all owned
	// BudgetPolicies.
	// +optional
	AggregateBudgetUsd string `json:"aggregateBudgetUsd,omitempty"`

	// PercentOfBudget — 0..200+. Computed from AggregateSpend /
	// AggregateBudget. When > 100 a TenantBudgetExceeded condition fires.
	// +optional
	PercentOfBudget int32 `json:"percentOfBudget,omitempty"`

	// LastReconciled timestamp.
	// +optional
	LastReconciled *metav1.Time `json:"lastReconciled,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=tnt
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Persona",type=string,JSONPath=`.spec.primaryPersona`
// +kubebuilder:printcolumn:name="Platforms",type=integer,JSONPath=`.status.platformCount`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyPlatformCount`
// +kubebuilder:printcolumn:name="Suspended",type=integer,JSONPath=`.status.suspendedPlatformCount`
// +kubebuilder:printcolumn:name="Spend",type=string,JSONPath=`.status.aggregateSpendUsd`
// +kubebuilder:printcolumn:name="Pct",type=integer,JSONPath=`.status.percentOfBudget`

// Tenant is the cluster-scoped organizational owner of one or more
// Platforms. Provides aggregate budget / readiness / suspension views and
// a single point for non-technical persona dashboards to land on.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList is the list-form of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
