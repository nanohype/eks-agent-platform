/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
)

// BatchJobSpec submits an Amazon Bedrock batch-inference job
// (CreateModelInvocationJob): an S3 JSONL of records in, an S3 JSONL of
// results out. One BatchJob maps to exactly one Bedrock job — there is no
// schedule; create a new CR for each run (the reconciler is idempotent on
// the spec, so re-applying the same CR never double-submits).
type BatchJobSpec struct {
	PlatformRef commonv1alpha1.LocalRef `json:"platformRef"`

	// ModelID is the Bedrock model id or inference-profile id the batch job
	// invokes (e.g. "anthropic.claude-3-5-sonnet-20241022-v2:0" or a
	// cross-region "us.anthropic.…" profile). Validated server-side by
	// Bedrock; kept a free string here like Identity.AllowedModels.
	// +kubebuilder:validation:MinLength=1
	ModelID string `json:"modelId"`

	// ModelInvocationType selects the record schema in the input JSONL —
	// raw InvokeModel bodies or Converse turns.
	// +kubebuilder:validation:Enum=InvokeModel;Converse
	// +kubebuilder:default=InvokeModel
	ModelInvocationType string `json:"modelInvocationType,omitempty"`

	// InputS3Uri is the s3:// URI of the input JSONL (or its prefix).
	// +kubebuilder:validation:Pattern=`^s3://.+`
	InputS3Uri string `json:"inputS3Uri"`

	// OutputS3Prefix is the s3:// prefix Bedrock writes results under.
	// +kubebuilder:validation:Pattern=`^s3://.+`
	OutputS3Prefix string `json:"outputS3Prefix"`

	// TimeoutHours bounds the job's runtime. Bedrock requires 24..168.
	// +kubebuilder:validation:Minimum=24
	// +kubebuilder:validation:Maximum=168
	// +kubebuilder:default=24
	TimeoutHours int32 `json:"timeoutHours,omitempty"`

	// ServiceRoleArnOverride replaces the operator-resolved Bedrock batch
	// service role (the role Bedrock assumes to read input / write output).
	// Normally empty — the reconciler injects the SSM-resolved role.
	// +optional
	ServiceRoleArnOverride string `json:"serviceRoleArnOverride,omitempty"`
}

// BatchJobStatus tracks the Bedrock job the reconciler submitted and polls.
type BatchJobStatus struct {
	// JobArn is the submitted job's ARN. Non-empty is the idempotency guard:
	// once set, the reconciler polls rather than re-submitting.
	// +optional
	JobArn string `json:"jobArn,omitempty"`

	// JobName is the deterministic, Bedrock-sanitized job name.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// Phase: Pending, Provisioning, Running, Succeeded, Failed, Stopped.
	// +optional
	Phase string `json:"phase,omitempty"`

	// SubmittedAt / CompletedAt timestamps.
	// +optional
	SubmittedAt *metav1.Time `json:"submittedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// OutputLocation is the s3:// URI Bedrock reports for the results.
	// +optional
	OutputLocation string `json:"outputLocation,omitempty"`

	// RecordCount / SucceededCount / FailedCount mirror Bedrock's
	// ProcessedRecordCount / SuccessRecordCount / ErrorRecordCount once the
	// job is running or terminal.
	// +optional
	RecordCount int64 `json:"recordCount,omitempty"`
	// +optional
	SucceededCount int64 `json:"succeededCount,omitempty"`
	// +optional
	FailedCount int64 `json:"failedCount,omitempty"`

	// Message carries the last Bedrock status / failure reason.
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=batch
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platformRef.name`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelId`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Output",type=string,JSONPath=`.status.outputLocation`

// BatchJob runs a single Amazon Bedrock batch-inference job for a Platform tenant.
type BatchJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BatchJobSpec   `json:"spec,omitempty"`
	Status BatchJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BatchJobList is the list-form of BatchJob.
type BatchJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BatchJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BatchJob{}, &BatchJobList{})
}
