/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// batchFinalizer ensures an in-flight Bedrock job is stopped before the
// BatchJob CR is removed.
const batchFinalizer = "agents.nanohype.dev/batch-finalizer"

// phaseBatchStopped is the BatchJob-local terminal phase for a job that was
// stopped (Bedrock "Stopped"). The shared phases.go set covers the rest.
const phaseBatchStopped = "Stopped"

// batchJobNameMaxLen is Bedrock's CreateModelInvocationJob JobName cap.
const batchJobNameMaxLen = 63

// errPlatformBatchNotFound is the sentinel for a BatchJob whose platformRef
// points at a Platform that doesn't exist. Surfaced as Pending — a Platform
// create event re-drives reconciliation, so we don't burn the workqueue.
var errPlatformBatchNotFound = errors.New("batch platformRef not found")

// resolveBatchPlatform fetches the referenced Platform (same shape as the
// Budget/Eval resolvers).
func (r *BatchJobReconciler) resolveBatchPlatform(ctx context.Context, bj *agentsv1alpha1.BatchJob) (*platformv1alpha1.Platform, error) {
	var p platformv1alpha1.Platform
	key := types.NamespacedName{Namespace: bj.Namespace, Name: bj.Spec.PlatformRef.Name}
	if err := r.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errPlatformBatchNotFound
		}
		return nil, fmt.Errorf("get platform %s: %w", key, err)
	}
	return &p, nil
}

// reconcileBatch submits the Bedrock job once (guarded by status.JobArn +
// a stable clientRequestToken) and then polls it. Returns the phase to
// write into status; missing-platform / not-yet-ready / no-AWS surface as
// Pending so the reconciler doesn't burn on backoff.
func (r *BatchJobReconciler) reconcileBatch(ctx context.Context, bj *agentsv1alpha1.BatchJob) (string, error) {
	platform, err := r.resolveBatchPlatform(ctx, bj)
	if err != nil {
		if errors.Is(err, errPlatformBatchNotFound) {
			return phasePending, nil
		}
		return "", err
	}
	// Don't submit for a half-provisioned or suspended tenant.
	if platform.Status.Phase != phaseReady {
		return phasePending, nil
	}
	// No Bedrock client (envtest / dev / --disable-aws) — can't submit.
	if r.Bedrock == nil {
		return phasePending, nil
	}

	roleARN := r.ServiceRoleARN
	if bj.Spec.ServiceRoleArnOverride != "" {
		roleARN = bj.Spec.ServiceRoleArnOverride
	}
	if roleARN == "" {
		return "", fmt.Errorf("no Bedrock batch service role configured; set the batch-runtime SSM output or spec.serviceRoleArnOverride")
	}

	// Submit once. status.JobArn is the idempotency guard across reconciles;
	// the clientRequestToken is the cross-the-status-write-race guard —
	// Bedrock collapses a re-submit carrying the same token.
	if bj.Status.JobArn == "" {
		jobName := batchJobName(bj)
		out, createErr := r.Bedrock.CreateModelInvocationJob(ctx, &bedrock.CreateModelInvocationJobInput{
			JobName:            aws.String(jobName),
			RoleArn:            aws.String(roleARN),
			ModelId:            aws.String(bj.Spec.ModelID),
			ClientRequestToken: aws.String(batchClientToken(bj)),
			InputDataConfig: &bedrocktypes.ModelInvocationJobInputDataConfigMemberS3InputDataConfig{
				Value: bedrocktypes.ModelInvocationJobS3InputDataConfig{S3Uri: aws.String(bj.Spec.InputS3Uri)},
			},
			OutputDataConfig: &bedrocktypes.ModelInvocationJobOutputDataConfigMemberS3OutputDataConfig{
				Value: bedrocktypes.ModelInvocationJobS3OutputDataConfig{S3Uri: aws.String(bj.Spec.OutputS3Prefix)},
			},
			TimeoutDurationInHours: aws.Int32(batchTimeoutHours(bj)),
			ModelInvocationType:    bedrocktypes.ModelInvocationType(batchInvocationType(bj)),
		})
		if createErr != nil {
			return "", fmt.Errorf("create bedrock batch job: %w", createErr)
		}
		bj.Status.JobArn = aws.ToString(out.JobArn)
		bj.Status.JobName = jobName
		bj.Status.SubmittedAt = ptrTimeNow()
		return phaseProvisioning, nil
	}

	// Poll the submitted job.
	got, getErr := r.Bedrock.GetModelInvocationJob(ctx, &bedrock.GetModelInvocationJobInput{
		JobIdentifier: aws.String(bj.Status.JobArn),
	})
	if getErr != nil {
		return "", fmt.Errorf("get bedrock batch job %s: %w", bj.Status.JobArn, getErr)
	}
	bj.Status.Message = aws.ToString(got.Message)
	bj.Status.RecordCount = aws.ToInt64(got.ProcessedRecordCount)
	bj.Status.SucceededCount = aws.ToInt64(got.SuccessRecordCount)
	bj.Status.FailedCount = aws.ToInt64(got.ErrorRecordCount)
	if uri := s3OutputURI(got.OutputDataConfig); uri != "" {
		bj.Status.OutputLocation = uri
	}
	phase := mapBedrockStatus(string(got.Status))
	if isTerminalBatchPhase(phase) && bj.Status.CompletedAt == nil {
		if got.EndTime != nil {
			t := metav1.NewTime(*got.EndTime)
			bj.Status.CompletedAt = &t
		} else {
			bj.Status.CompletedAt = ptrTimeNow()
		}
	}
	return phase, nil
}

// stopBatchJob is the finalizer counterpart — best-effort stop of an
// in-flight Bedrock job. A stop failure is logged but never blocks deletion:
// a leaked job is bounded by its TimeoutDurationInHours, whereas a trapped
// finalizer is not. It returns nothing precisely because deletion must not
// hinge on the stop succeeding.
func (r *BatchJobReconciler) stopBatchJob(ctx context.Context, bj *agentsv1alpha1.BatchJob) {
	if r.Bedrock == nil || bj.Status.JobArn == "" || isTerminalBatchPhase(bj.Status.Phase) {
		return
	}
	if _, err := r.Bedrock.StopModelInvocationJob(ctx, &bedrock.StopModelInvocationJobInput{
		JobIdentifier: aws.String(bj.Status.JobArn),
	}); err != nil {
		log.FromContext(ctx).Error(err, "stop bedrock batch job failed; allowing CR deletion (job is timeout-bounded)", "jobArn", bj.Status.JobArn)
	}
}

// applyBatchStatus writes the computed phase + the BatchJobReconciled
// condition, persisting any status fields set during reconcileBatch.
func (r *BatchJobReconciler) applyBatchStatus(ctx context.Context, bj *agentsv1alpha1.BatchJob, phase string) error {
	bj.Status.Phase = phase
	cond := metav1.Condition{
		Type:               "BatchJobReconciled",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("bedrock batch job phase=%s", phase),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: bj.Generation,
	}
	if phase == phaseFailed {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "JobFailed"
		if bj.Status.Message != "" {
			cond.Message = bj.Status.Message
		}
	}
	upsertCondition(&bj.Status.Conditions, cond)
	return r.Status().Update(ctx, bj)
}

// applyBatchStatusError records a reconcile-error condition without
// clobbering the rest of status.
func (r *BatchJobReconciler) applyBatchStatusError(ctx context.Context, bj *agentsv1alpha1.BatchJob, reason string, cause error) error {
	upsertCondition(&bj.Status.Conditions, metav1.Condition{
		Type:               "BatchJobReconciled",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: bj.Generation,
	})
	return r.Status().Update(ctx, bj)
}

// mapBedrockStatus folds Bedrock's ModelInvocationJobStatus values onto the
// shared reconciler phase vocabulary. Unknown / transitional statuses map to
// Provisioning so the reconciler keeps polling rather than going terminal.
func mapBedrockStatus(status string) string {
	switch status {
	case "Submitted", "Validating", "Scheduled":
		return phaseProvisioning
	case "InProgress", "Stopping":
		return phaseRunning
	case "Completed", "PartiallyCompleted":
		return phaseSucceeded
	case "Failed", "Expired":
		return phaseFailed
	case "Stopped":
		return phaseBatchStopped
	default:
		return phaseProvisioning
	}
}

func isTerminalBatchPhase(phase string) bool {
	switch phase {
	case phaseSucceeded, phaseFailed, phaseBatchStopped:
		return true
	default:
		return false
	}
}

// s3OutputURI extracts the S3 URI from the Bedrock output-data-config union.
func s3OutputURI(cfg bedrocktypes.ModelInvocationJobOutputDataConfig) string {
	if m, ok := cfg.(*bedrocktypes.ModelInvocationJobOutputDataConfigMemberS3OutputDataConfig); ok {
		return aws.ToString(m.Value.S3Uri)
	}
	return ""
}

// batchJobName is the deterministic, Bedrock-sanitized job name
// (<platform>-<crname>, hash-truncated to 63 chars when too long).
func batchJobName(bj *agentsv1alpha1.BatchJob) string {
	base := bj.Spec.PlatformRef.Name + "-" + bj.Name
	clean := sanitizeBedrockName(base)
	if len(clean) <= batchJobNameMaxLen {
		return clean
	}
	budget := batchJobNameMaxLen - 1 - 8 // hyphen + 8 hex of the fnv hash
	return fmt.Sprintf("%s-%08x", clean[:budget], fnv1a64(base)&0xffffffff)
}

// sanitizeBedrockName coerces s into Bedrock's job-name charset
// ([a-zA-Z0-9](-*[a-zA-Z0-9+.]){0,62}). Platform + CR names are RFC-1123
// (lowercase alnum + '-'), already in-charset; this is defense in depth.
func sanitizeBedrockName(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range []byte(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '+':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "batch"
	}
	if !isASCIIAlnum(out[0]) {
		out = append([]byte{'b'}, out...)
	}
	return string(out)
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// batchClientToken is a stable idempotency token for CreateModelInvocationJob.
// Keyed on identity + input so a retried reconcile of the same spec collapses
// server-side even if a prior status write (status.JobArn) was lost.
func batchClientToken(bj *agentsv1alpha1.BatchJob) string {
	seed := fmt.Sprintf("%s/%s/%d/%s", bj.Namespace, bj.Name, bj.Generation, bj.Spec.InputS3Uri)
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%x", sum)
}

func batchTimeoutHours(bj *agentsv1alpha1.BatchJob) int32 {
	if bj.Spec.TimeoutHours <= 0 {
		return 24
	}
	return bj.Spec.TimeoutHours
}

func batchInvocationType(bj *agentsv1alpha1.BatchJob) string {
	if bj.Spec.ModelInvocationType == "" {
		return "InvokeModel"
	}
	return bj.Spec.ModelInvocationType
}

func ptrTimeNow() *metav1.Time {
	t := metav1.Now()
	return &t
}
