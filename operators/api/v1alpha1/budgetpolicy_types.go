/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BudgetPolicySpec sets monthly spend caps per Platform.
type BudgetPolicySpec struct {
	PlatformRef LocalRef `json:"platformRef"`

	// MonthlyUsd is the soft threshold expressed as a decimal-string USD amount
	// (e.g. "2500", "1500.50"). KillSwitch fires at 120% of this. Modeled as
	// string for symmetry with Status.CurrentSpendUsd and so future v1 can
	// support fractional cents without a lossy int32 → string conversion. The
	// pattern enforces non-negative decimal with optional 2-digit fraction.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,2})?$`
	// +kubebuilder:validation:MinLength=1
	MonthlyUsd string `json:"monthlyUsd"`

	// AlertThresholdsPercent — fire WarnEvent at these % of the threshold.
	// +kubebuilder:default={50,80,100}
	// +optional
	AlertThresholdsPercent []int32 `json:"alertThresholdsPercent,omitempty"`

	// KillSwitchEnabled — when false, breach at 120% is logged but not acted on.
	// Use sparingly; SOC2 platforms must keep this true.
	// +kubebuilder:default=true
	KillSwitchEnabled bool `json:"killSwitchEnabled"`
}

// BudgetPolicyStatus surfaces the latest spend reading. The budget reconciler
// updates this on every tick (hourly in prod, 5m in dev) with current spend,
// percent-of-budget, the alert thresholds crossed, and reconcile conditions.
type BudgetPolicyStatus struct {
	// CurrentSpendUsd is the most recent spend snapshot.
	// +optional
	CurrentSpendUsd string `json:"currentSpendUsd,omitempty"`

	// PercentOfBudget — 0..200+.
	// +optional
	PercentOfBudget int32 `json:"percentOfBudget,omitempty"`

	// LastReconciled timestamp.
	// +optional
	LastReconciled *metav1.Time `json:"lastReconciled,omitempty"`

	// KillSwitchFiredAt — non-null if the kill-switch fired and the platform
	// is currently suspended.
	// +optional
	KillSwitchFiredAt *metav1.Time `json:"killSwitchFiredAt,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=budget
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platformRef.name`
// +kubebuilder:printcolumn:name="MonthlyUSD",type=string,JSONPath=`.spec.monthlyUsd`
// +kubebuilder:printcolumn:name="Spend",type=string,JSONPath=`.status.currentSpendUsd`
// +kubebuilder:printcolumn:name="Pct",type=integer,JSONPath=`.status.percentOfBudget`

// BudgetPolicy caps monthly spend per Platform and triggers the kill-switch at 120% of the threshold.
type BudgetPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BudgetPolicySpec   `json:"spec,omitempty"`
	Status BudgetPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BudgetPolicyList is the list-form of BudgetPolicy.
type BudgetPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BudgetPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BudgetPolicy{}, &BudgetPolicyList{})
}
