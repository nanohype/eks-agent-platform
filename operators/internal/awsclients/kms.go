/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// KMS is the slice of the aws-sdk-go-v2 KMS client the operator uses.
// All grant management — no key creation, no Encrypt/Decrypt. The
// operator never sees plaintext content (see ADR 0003).
type KMS interface {
	CreateGrant(ctx context.Context, params *kms.CreateGrantInput, optFns ...func(*kms.Options)) (*kms.CreateGrantOutput, error)
	ListGrants(ctx context.Context, params *kms.ListGrantsInput, optFns ...func(*kms.Options)) (*kms.ListGrantsOutput, error)
	RevokeGrant(ctx context.Context, params *kms.RevokeGrantInput, optFns ...func(*kms.Options)) (*kms.RevokeGrantOutput, error)
}

var _ KMS = (*kms.Client)(nil)
