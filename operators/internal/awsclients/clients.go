/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	smithymiddleware "github.com/aws/smithy-go/middleware"
)

// Clients holds the AWS service interfaces the operator depends on. Each
// reconciler receives this struct (or a subset interface) so tests can
// inject in-memory fakes. Any individual field may be nil — reconcilers
// short-circuit AWS-side work when the relevant client isn't wired
// (envtest, dev clusters without IRSA, partial-config dev/staging).
type Clients struct {
	IAM         IAM
	EKS         EKS
	SSM         SSM
	KMS         KMS
	S3          S3
	Athena      Athena
	CloudWatch  CloudWatch
	EventBridge EventBridge
	Bedrock     Bedrock
}

// awsHTTPTimeout bounds every AWS SDK request. controller-runtime does not
// decorate the reconcile context with a per-call deadline, and the SDK's
// default transport sets no overall request timeout — so without this a
// connection that establishes then stalls before responding would pin a
// bounded reconcile worker indefinitely, eventually starving the pool. 30s
// comfortably covers the slowest single control-plane call; the Athena poll
// path bounds its own multi-call loop separately (budget_reconcile.go).
const awsHTTPTimeout = 30 * time.Second

// awsOpTimeout bounds a whole AWS operation — the call plus all of its automatic
// retries — not just a single HTTP request the way awsHTTPTimeout does. Without
// it, a method that keeps failing transiently could retry up to the SDK's
// attempt cap (~3 × awsHTTPTimeout + backoff) before returning, outlasting the
// reconcile it runs inside. Applied as an Initialize-step middleware so it
// covers every operation on every client from one place. It's a ceiling: a
// caller that passes a shorter deadline still wins.
const awsOpTimeout = 60 * time.Second

// withOperationTimeout returns an API-options hook that wraps each operation's
// context with a deadline. Initialize runs once per operation, before the retry
// loop, so the deadline spans every attempt.
func withOperationTimeout(d time.Duration) func(*smithymiddleware.Stack) error {
	return func(stack *smithymiddleware.Stack) error {
		return stack.Initialize.Add(smithymiddleware.InitializeMiddlewareFunc(
			"OperationTimeout",
			func(ctx context.Context, in smithymiddleware.InitializeInput, next smithymiddleware.InitializeHandler) (smithymiddleware.InitializeOutput, smithymiddleware.Metadata, error) {
				ctx, cancel := context.WithTimeout(ctx, d)
				defer cancel()
				return next.HandleInitialize(ctx, in)
			},
		), smithymiddleware.Before)
	}
}

// New builds a Clients backed by the default credential chain (IRSA via
// fromContainerCredentials → fromEnv → fromInstanceProfile). Region is
// resolved from the same chain unless explicitly passed.
func New(ctx context.Context, region string) (*Clients, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithHTTPClient(awshttp.NewBuildableClient().WithTimeout(awsHTTPTimeout)),
		awsconfig.WithAPIOptions([]func(*smithymiddleware.Stack) error{withOperationTimeout(awsOpTimeout)}),
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &Clients{
		IAM:         iam.NewFromConfig(cfg),
		EKS:         eks.NewFromConfig(cfg),
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
