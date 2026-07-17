/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// EvalSuiteSpec defines a periodic evaluation run against an AgentFleet.
// +kubebuilder:validation:XValidation:rule="!(has(self.casesFromManifest) && self.casesFromManifest != \"\" && has(self.cases) && size(self.cases) > 0)",message="cases and casesFromManifest are mutually exclusive"
type EvalSuiteSpec struct {
	PlatformRef commonv1alpha1.LocalRef `json:"platformRef"`

	// AgentFleetRef targets the fleet whose agents are under test.
	AgentFleetRef commonv1alpha1.LocalRef `json:"agentFleetRef"`

	// Schedule (cron) — when to run the suite. Empty = manual only.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Cases is the list of test cases (input prompt + expected criteria).
	// In production these are typically loaded from an S3 manifest; this
	// inline list is for small / dev suites.
	// +optional
	Cases []EvalCase `json:"cases,omitempty"`

	// CasesFromManifest loads from `eval-reports/<platform>/manifests/<name>.json`
	// in the eval-reports S3 bucket.
	// +optional
	CasesFromManifest string `json:"casesFromManifest,omitempty"`

	// PassThreshold (0..1) is the required mean score for the run to be
	// marked passing. Argo Rollouts AnalysisTemplate consumes this signal.
	// Modeled as a string so reviewers see decimals in `kubectl get -o yaml`
	// without int<->float coercion surprises; pattern enforces 0.0 .. 1.0.
	// +kubebuilder:default="0.85"
	// +kubebuilder:validation:Pattern=`^(0(\.[0-9]+)?|1(\.0+)?)$`
	PassThreshold string `json:"passThreshold,omitempty"`
}

// EvalCase is a single test case. The assertion fields it sets determine its
// kind — the runner has no separate discriminator:
//
//   - Golden case: sets ExpectContains (and optionally MaxLatencyMs /
//     MaxCostUsd). Passes when the agent's output contains every listed
//     substring and stays within the latency/cost ceilings.
//   - Adversarial / injection case: sets ExpectNotContains and/or
//     ExpectRefusal. Passes when the output leaks none of the forbidden
//     substrings and — when ExpectRefusal is set — the agent declined
//     (a guardrail intervened, or the output matched a refusal).
//
// A case may combine both families (e.g. a jailbreak attempt that must be
// refused AND must not echo a secret). All assertions present must hold.
type EvalCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`

	// ExpectContains: the output must contain every one of these substrings
	// (golden / positive assertion). Empty = no positive-content assertion.
	// +optional
	ExpectContains []string `json:"expectContains,omitempty"`

	// ExpectNotContains: the output must contain none of these substrings
	// (adversarial / data-leak assertion — e.g. a secret, PII, or a phrase
	// that would indicate the agent complied with an injection). Empty = no
	// forbidden-content assertion.
	// +optional
	ExpectNotContains []string `json:"expectNotContains,omitempty"`

	// ExpectRefusal: when true, the case passes only if the agent declined —
	// either the model gateway reported a guardrail intervention, or the
	// output matched a refusal. Use for adversarial prompts that should be
	// blocked rather than answered.
	// +optional
	ExpectRefusal bool `json:"expectRefusal,omitempty"`

	// MaxLatencyMs: if set (>0), the case fails when the observed round-trip
	// latency exceeds this ceiling.
	// +optional
	MaxLatencyMs int32 `json:"maxLatencyMs,omitempty"`

	// MaxCostUsd: if set, the case fails when the observed per-call cost
	// exceeds this ceiling. A model with no pricing entry (unpriced) fails
	// this assertion closed rather than passing on a misleading $0.
	// +optional
	MaxCostUsd string `json:"maxCostUsd,omitempty"`
}

// EvalSuiteStatus reports the latest run.
type EvalSuiteStatus struct {
	// LastRunAt timestamp.
	// +optional
	LastRunAt *metav1.Time `json:"lastRunAt,omitempty"`

	// LastScore (mean across cases, 0..1).
	// +optional
	LastScore string `json:"lastScore,omitempty"`

	// LastReportURL (s3:// URL to the rendered HTML report).
	// +optional
	LastReportURL string `json:"lastReportUrl,omitempty"`

	// Phase: Pending, Running, Passed, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=eval
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Fleet",type=string,JSONPath=`.spec.agentFleetRef.name`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="LastScore",type=string,JSONPath=`.status.lastScore`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// EvalSuite is a scheduled evaluation run against an AgentFleet's agents.
type EvalSuite struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EvalSuiteSpec   `json:"spec,omitempty"`
	Status EvalSuiteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EvalSuiteList is the list-form of EvalSuite.
type EvalSuiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvalSuite `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EvalSuite{}, &EvalSuiteList{})
}
