/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// fakeBedrock is an in-memory awsclients.Bedrock covering the batch job
// lifecycle the reconciler drives (submit → poll → stop).
type fakeBedrock struct {
	status     string
	createErr  error
	getErr     error
	created    []bedrock.CreateModelInvocationJobInput
	stopCalled bool
}

func (f *fakeBedrock) CreateModelInvocationJob(_ context.Context, in *bedrock.CreateModelInvocationJobInput, _ ...func(*bedrock.Options)) (*bedrock.CreateModelInvocationJobOutput, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, *in)
	return &bedrock.CreateModelInvocationJobOutput{JobArn: aws.String("arn:aws:bedrock:us-west-2:1:model-invocation-job/j1")}, nil
}

func (f *fakeBedrock) GetModelInvocationJob(_ context.Context, _ *bedrock.GetModelInvocationJobInput, _ ...func(*bedrock.Options)) (*bedrock.GetModelInvocationJobOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &bedrock.GetModelInvocationJobOutput{
		Status:               bedrocktypes.ModelInvocationJobStatus(f.status),
		ProcessedRecordCount: aws.Int64(100),
		SuccessRecordCount:   aws.Int64(98),
		OutputDataConfig: &bedrocktypes.ModelInvocationJobOutputDataConfigMemberS3OutputDataConfig{
			Value: bedrocktypes.ModelInvocationJobS3OutputDataConfig{S3Uri: aws.String("s3://out/j1/")},
		},
	}, nil
}

func (f *fakeBedrock) StopModelInvocationJob(_ context.Context, _ *bedrock.StopModelInvocationJobInput, _ ...func(*bedrock.Options)) (*bedrock.StopModelInvocationJobOutput, error) {
	f.stopCalled = true
	return &bedrock.StopModelInvocationJobOutput{}, nil
}

func batchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := agentsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReconcileBatch(t *testing.T) {
	s := batchScheme(t)

	bj := func() *agentsv1alpha1.BatchJob {
		j := batchJob("nightly", ctrlTestPlatform)
		j.Namespace = ctrlTestNS
		j.Spec.ModelID = "anthropic.claude-sonnet-4-6-v1:0"
		j.Spec.OutputS3Prefix = "s3://out/"
		return j
	}

	t.Run("missing platform is pending", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &BatchJobReconciler{Client: cl, Scheme: s, Bedrock: &fakeBedrock{}}
		if phase, err := r.reconcileBatch(context.Background(), bj()); err != nil || phase != phasePending {
			t.Fatalf("missing platform: got (%q, %v)", phase, err)
		}
	})

	t.Run("no bedrock client is pending", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(readyPlatformIn()).Build()
		r := &BatchJobReconciler{Client: cl, Scheme: s} // no Bedrock
		if phase, err := r.reconcileBatch(context.Background(), bj()); err != nil || phase != phasePending {
			t.Fatalf("no bedrock: got (%q, %v)", phase, err)
		}
	})

	t.Run("no service role errors", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(readyPlatformIn()).Build()
		r := &BatchJobReconciler{Client: cl, Scheme: s, Bedrock: &fakeBedrock{}} // no ServiceRoleARN
		if _, err := r.reconcileBatch(context.Background(), bj()); err == nil {
			t.Fatal("a missing batch service role must error")
		}
	})

	t.Run("submits once then reports Provisioning", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(readyPlatformIn()).Build()
		fb := &fakeBedrock{}
		r := &BatchJobReconciler{Client: cl, Scheme: s, Bedrock: fb, ServiceRoleARN: "arn:aws:iam::1:role/batch"}
		j := bj()
		phase, err := r.reconcileBatch(context.Background(), j)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		if phase != phaseProvisioning || j.Status.JobArn == "" {
			t.Errorf("submit: phase=%q jobArn=%q", phase, j.Status.JobArn)
		}
		if len(fb.created) != 1 {
			t.Errorf("expected exactly one CreateModelInvocationJob, got %d", len(fb.created))
		}
	})

	t.Run("polls a submitted job and maps a terminal status", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(readyPlatformIn()).Build()
		fb := &fakeBedrock{status: "Completed"}
		r := &BatchJobReconciler{Client: cl, Scheme: s, Bedrock: fb, ServiceRoleARN: "arn:aws:iam::1:role/batch"}
		j := bj()
		j.Status.JobArn = "arn:aws:bedrock:us-west-2:1:model-invocation-job/j1" // already submitted
		phase, err := r.reconcileBatch(context.Background(), j)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if phase != phaseSucceeded {
			t.Errorf("Completed must map to Succeeded, got %q", phase)
		}
		if j.Status.OutputLocation != "s3://out/j1/" || j.Status.CompletedAt == nil {
			t.Errorf("poll status writeback: output=%q completedAt=%v", j.Status.OutputLocation, j.Status.CompletedAt)
		}
		if len(fb.created) != 0 {
			t.Error("a job with a JobArn must not be resubmitted")
		}
	})

	t.Run("create error propagates", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(readyPlatformIn()).Build()
		fb := &fakeBedrock{createErr: errors.New("throttled")}
		r := &BatchJobReconciler{Client: cl, Scheme: s, Bedrock: fb, ServiceRoleARN: "arn:aws:iam::1:role/batch"}
		if _, err := r.reconcileBatch(context.Background(), bj()); err == nil {
			t.Fatal("a CreateModelInvocationJob error must propagate")
		}
	})
}

func TestStopBatchJob(t *testing.T) {
	fb := &fakeBedrock{}
	r := &BatchJobReconciler{Bedrock: fb}
	// A running job with a JobArn gets stopped.
	j := batchJob("j", ctrlTestPlatform)
	j.Status.JobArn = "arn:job/1"
	j.Status.Phase = phaseRunning
	r.stopBatchJob(context.Background(), j)
	if !fb.stopCalled {
		t.Error("an in-flight job must be stopped on finalize")
	}
	// A terminal job is not stopped.
	fb.stopCalled = false
	j.Status.Phase = phaseSucceeded
	r.stopBatchJob(context.Background(), j)
	if fb.stopCalled {
		t.Error("a terminal job must not be stopped")
	}
}

func TestApplyBatchStatusError(t *testing.T) {
	s := batchScheme(t)
	j := batchJob("j", ctrlTestPlatform)
	j.Namespace = ctrlTestNS
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(j).WithStatusSubresource(j).Build()
	r := &BatchJobReconciler{Client: cl, Scheme: s}
	if err := r.applyBatchStatusError(context.Background(), j, "SubmitFailed", errors.New("boom")); err != nil {
		t.Fatalf("applyBatchStatusError: %v", err)
	}
	var got agentsv1alpha1.BatchJob
	if err := cl.Get(context.Background(), types.NamespacedName{Name: j.Name, Namespace: j.Namespace}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Conditions) == 0 || got.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Errorf("expected a False BatchJobReconciled condition, got %+v", got.Status.Conditions)
	}
}
