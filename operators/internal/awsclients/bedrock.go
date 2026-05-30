/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/bedrock"
)

// Bedrock is the slice of aws-sdk-go-v2/bedrock — the control-plane client,
// NOT bedrockruntime — the BatchJob reconciler uses to manage batch
// model-invocation jobs. Only the job lifecycle is exposed: the operator
// submits a job, polls it, and stops it on delete. The input/output buckets
// and the Bedrock batch service role are provisioned by
// terraform/components/batch-runtime, not here.
type Bedrock interface {
	CreateModelInvocationJob(ctx context.Context, params *bedrock.CreateModelInvocationJobInput, optFns ...func(*bedrock.Options)) (*bedrock.CreateModelInvocationJobOutput, error)
	GetModelInvocationJob(ctx context.Context, params *bedrock.GetModelInvocationJobInput, optFns ...func(*bedrock.Options)) (*bedrock.GetModelInvocationJobOutput, error)
	StopModelInvocationJob(ctx context.Context, params *bedrock.StopModelInvocationJobInput, optFns ...func(*bedrock.Options)) (*bedrock.StopModelInvocationJobOutput, error)
}

var _ Bedrock = (*bedrock.Client)(nil)
