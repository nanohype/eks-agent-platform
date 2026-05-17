/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package awsclients defines minimal AWS service interfaces consumed by the
// operator's reconcilers. Interfaces, not concrete clients, are passed to
// reconcilers so tests can inject in-memory fakes without spinning up
// LocalStack or hitting real AWS.
//
// One file per service. Each file declares an interface naming only the
// methods we actually call, plus a concrete adapter that wraps the
// aws-sdk-go-v2 client.
package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// IAM is the slice of the aws-sdk-go-v2 IAM client that the operator uses.
// Keep this list minimal — every method added expands the surface that
// fakes must implement and that ADR 0003 must document.
type IAM interface {
	CreateRole(ctx context.Context, params *iam.CreateRoleInput, optFns ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	GetRole(ctx context.Context, params *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	DeleteRole(ctx context.Context, params *iam.DeleteRoleInput, optFns ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
	TagRole(ctx context.Context, params *iam.TagRoleInput, optFns ...func(*iam.Options)) (*iam.TagRoleOutput, error)
	UpdateAssumeRolePolicy(ctx context.Context, params *iam.UpdateAssumeRolePolicyInput, optFns ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error)
	AttachRolePolicy(ctx context.Context, params *iam.AttachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	DetachRolePolicy(ctx context.Context, params *iam.DetachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	ListAttachedRolePolicies(ctx context.Context, params *iam.ListAttachedRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
}

// Compile-time check that the real aws-sdk-go-v2 client satisfies our
// interface. If aws-sdk-go-v2 adds a required field to a method we'd hit
// here at build time rather than at runtime.
var _ IAM = (*iam.Client)(nil)
