/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// SLI declares the good-events/valid-events ratio an objective is measured on.
// The reconciler builds the PromQL from these fields rather than accepting a
// query — a raw expression would be an injection seam into a request the
// operator signs with the platform's own credentials, and the shapes are fully
// determined by the observability-slo standard's sli_types anyway.
type SLI struct {
	// Type selects the ratio shape. availability divides an errors counter by a
	// requests counter; latency divides a duration histogram's under-threshold
	// bucket by its count.
	// +kubebuilder:validation:Enum=availability;latency
	Type string `json:"type"`

	// Metric is the base series name in Prometheus form — the OTLP service name
	// with dashes normalized to underscores, without the _errors_total /
	// _requests_total / _request_duration_seconds_* suffix the Type implies
	// (e.g. "incident_response_webhook").
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_]*$`
	// +kubebuilder:validation:MaxLength=200
	Metric string `json:"metric"`

	// Selector narrows the SLI to a subset of series as an exact-match label
	// set. Rendered into the query as label="value" with values escaped. Keys
	// are Prometheus label names; a raw matcher string is deliberately not
	// accepted.
	// +optional
	Selector map[string]string `json:"selector,omitempty"`

	// ThresholdSeconds is the histogram bucket boundary a latency SLI counts as
	// good, as a decimal-string of seconds ("0.5"). Required for
	// type=latency, ignored for type=availability. It must name a bucket the
	// histogram actually publishes — an le value with no matching bucket yields
	// an empty result, which the reconciler reports as NoData rather than as a
	// healthy zero.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	// +optional
	ThresholdSeconds string `json:"thresholdSeconds,omitempty"`
}

// SLOPolicySpec declares one service-level objective for a Platform and what
// the control loop does when its error budget burns too fast.
type SLOPolicySpec struct {
	PlatformRef commonv1alpha1.LocalRef `json:"platformRef"`

	// SLI is the ratio this objective is measured on.
	SLI SLI `json:"sli"`

	// Objective is the target good-event ratio as a decimal string ("0.999").
	// The error budget is 1 - Objective, and the burn rate is the observed error
	// ratio over a window divided by that budget. Modeled as a string for the
	// same reason BudgetPolicy.monthlyUsd is: a float64 round-trip through JSON
	// would perturb the denominator every burn-rate alert divides by. Bounded
	// below 1 because an objective of 1 leaves a zero budget and an infinite
	// burn rate.
	// +kubebuilder:validation:Pattern=`^0\.[0-9]{1,6}$`
	Objective string `json:"objective"`

	// OnPageTierBreach is the automated action taken when a page-tier burn-rate
	// window pair trips.
	//
	//	HoldRollout — patch a deny syncWindow onto the tenant's ArgoCD
	//	              AppProject so a bad rollout stops advancing. The window
	//	              leaves manualSync open, so an operator can still push a
	//	              fix by hand. Reversed automatically once the burn clears.
	//	None        — evaluate, publish, and page, but take no cluster action.
	//
	// +kubebuilder:validation:Enum=HoldRollout;None
	// +kubebuilder:default=HoldRollout
	// +optional
	OnPageTierBreach string `json:"onPageTierBreach,omitempty"`
}

// SLOPolicyStatus surfaces the most recent burn-rate evaluation. The SLO
// reconciler rewrites it on every tick. It is the operator's single evaluation
// of this objective: kube-state-metrics projects these fields, so the paging
// alert reads the number computed here instead of re-deriving the same PromQL
// against the same data.
type SLOPolicyStatus struct {
	// ErrorRatios is the observed error ratio for each queried window, keyed by
	// the window ("5m", "1h", "3d", "30d", …) with a decimal-string value.
	// Present so an operator can see which window tripped, and by how much,
	// without re-running the queries by hand.
	// +optional
	ErrorRatios map[string]string `json:"errorRatios,omitempty"`

	// PageTierBreachRatio is how close the page tier is to breaching, as a
	// decimal string, normalized so one threshold covers every window: across
	// each page-tier window pair it takes min(long burn rate, short burn rate)
	// divided by that pair's factor, and reports the largest. 1.0 means a pair
	// is exactly at its factor; above 1.0 is a breach. Normalized rather than
	// raw because the page tier has two pairs with different factors (14.4 and
	// 6), so no single raw burn rate can be compared against one number.
	// +optional
	PageTierBreachRatio string `json:"pageTierBreachRatio,omitempty"`

	// TicketTierBreachRatio is the same normalized measure over the ticket-tier
	// window pairs (factors 3 and 1).
	// +optional
	TicketTierBreachRatio string `json:"ticketTierBreachRatio,omitempty"`

	// ErrorBudgetRemaining is the fraction of the SLO window's error budget
	// still unspent, as a decimal string clamped to [0,1]: 1 - (error ratio over
	// the 30d SLO window / error budget). Measured over the full window the
	// standard defines rather than extrapolated from a burn window, so the
	// gauge means what it says.
	// +optional
	ErrorBudgetRemaining string `json:"errorBudgetRemaining,omitempty"`

	// BreachedWindow names the long window whose pair tripped at the highest
	// severity ("1h", "6h", "1d", "3d"). Empty when nothing is breaching.
	// +optional
	BreachedWindow string `json:"breachedWindow,omitempty"`

	// Severity is the tier of the current breach: critical for a page-tier
	// window pair, warning for a ticket-tier pair, empty when healthy.
	// +kubebuilder:validation:Enum=critical;warning;""
	// +optional
	Severity string `json:"severity,omitempty"`

	// LastReconciled timestamp.
	// +optional
	LastReconciled *metav1.Time `json:"lastReconciled,omitempty"`

	// BreachFiredAt is when the current unbroken breach episode was first
	// published to the kill-switch bus. Cleared when the burn clears, so a
	// later breach publishes again rather than being swallowed as a duplicate.
	// +optional
	BreachFiredAt *metav1.Time `json:"breachFiredAt,omitempty"`

	// HoldEngagedAt is when this reconciler decided to hold the tenant's
	// rollout. It is the decision, not the effect: the Platform reconciler is
	// the only writer of the AppProject, and it renders the deny syncWindow from
	// this field. Non-null means a hold is called for.
	// +optional
	HoldEngagedAt *metav1.Time `json:"holdEngagedAt,omitempty"`

	// HoldObservedAt is when the deny syncWindow was last actually seen on the
	// tenant's AppProject. Engaging a hold is not the same as holding: if this
	// stays null past the grace window while HoldEngagedAt is set, the hold is
	// not landing and the HoldUnobserved condition says so.
	// +optional
	HoldObservedAt *metav1.Time `json:"holdObservedAt,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=slo
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platformRef.name`
// +kubebuilder:printcolumn:name="Objective",type=string,JSONPath=`.spec.objective`
// +kubebuilder:printcolumn:name="PageBurn",type=string,JSONPath=`.status.pageTierBreachRatio`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.status.severity`
// +kubebuilder:printcolumn:name="Held",type=date,JSONPath=`.status.holdEngagedAt`

// SLOPolicy declares a Platform's service-level objective and turns a page-tier
// error-budget burn into a platform action: an event on the kill-switch bus and
// a hold on the tenant's rollout.
type SLOPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SLOPolicySpec   `json:"spec,omitempty"`
	Status SLOPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SLOPolicyList is the list-form of SLOPolicy.
type SLOPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SLOPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SLOPolicy{}, &SLOPolicyList{})
}
