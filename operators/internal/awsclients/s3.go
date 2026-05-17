/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3 is the slice of the aws-sdk-go-v2 S3 client used to mutate the
// artifacts bucket policy. No GetObject/PutObject — the operator never
// reads or writes tenant data; tenants do that under their own IRSA.
type S3 interface {
	GetBucketPolicy(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error)
	PutBucketPolicy(ctx context.Context, params *s3.PutBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
}

var _ S3 = (*s3.Client)(nil)
