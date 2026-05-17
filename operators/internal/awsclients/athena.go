/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/athena"
)

// Athena is the slice of aws-sdk-go-v2/athena the Budget reconciler uses
// to query the Cost & Usage Report via the cost-pipeline workgroup. Only
// the query lifecycle (Start/Get/Results) is exposed — the operator never
// creates workgroups or named queries; those are managed by
// terraform/components/cost-pipeline.
type Athena interface {
	StartQueryExecution(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error)
	GetQueryExecution(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error)
	GetQueryResults(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error)
	// StopQueryExecution is invoked on context cancellation / poll timeout
	// so we don't bill for an orphan query whose result will be discarded.
	StopQueryExecution(ctx context.Context, params *athena.StopQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StopQueryExecutionOutput, error)
}

var _ Athena = (*athena.Client)(nil)
