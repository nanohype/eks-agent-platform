/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// SSM is the slice of the aws-sdk-go-v2 SSM client we use to read operator
// config at startup (operator-role ARN, tenant baseline policy ARN,
// artifacts bucket name, baseline guardrail ID, etc.).
type SSM interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	GetParametersByPath(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

var _ SSM = (*ssm.Client)(nil)
