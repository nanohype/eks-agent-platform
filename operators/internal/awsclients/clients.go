/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Clients holds the AWS service interfaces the operator depends on. Each
// reconciler receives this struct (or a subset interface) so tests can
// inject in-memory fakes. Any individual field may be nil — reconcilers
// short-circuit AWS-side work when the relevant client isn't wired
// (envtest, dev clusters without IRSA, partial-config dev/staging).
type Clients struct {
	IAM         IAM
	SSM         SSM
	KMS         KMS
	S3          S3
	Athena      Athena
	CloudWatch  CloudWatch
	EventBridge EventBridge
	Bedrock     Bedrock
}

// New builds a Clients backed by the default credential chain (IRSA via
// fromContainerCredentials → fromEnv → fromInstanceProfile). Region is
// resolved from the same chain unless explicitly passed.
func New(ctx context.Context, region string) (*Clients, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &Clients{
		IAM:         iam.NewFromConfig(cfg),
		SSM:         ssm.NewFromConfig(cfg),
		KMS:         kms.NewFromConfig(cfg),
		S3:          s3.NewFromConfig(cfg),
		Athena:      athena.NewFromConfig(cfg),
		CloudWatch:  cloudwatch.NewFromConfig(cfg),
		EventBridge: eventbridge.NewFromConfig(cfg),
		Bedrock:     bedrock.NewFromConfig(cfg),
	}, nil
}

// EnsureRegion is exported for tests that want to verify a custom region
// override survives Clients construction. Returns the AWS config's region.
func EnsureRegion(_ context.Context, c aws.Config) string {
	return c.Region
}
