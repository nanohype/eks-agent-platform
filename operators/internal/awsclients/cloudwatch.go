/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

// CloudWatch is the slice of aws-sdk-go-v2/cloudwatch the Budget
// reconciler uses to read Bedrock invocation metrics for in-flight spend
// estimation (anything since the last Athena/CUR partition).
type CloudWatch interface {
	GetMetricData(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

var _ CloudWatch = (*cloudwatch.Client)(nil)
