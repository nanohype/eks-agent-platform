/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/eks"
)

// EKS is the slice of the aws-sdk-go-v2 EKS client the operator uses to manage
// the Pod Identity association that binds a tenant ServiceAccount to its IAM
// role. Keep this list minimal — every method added expands the surface that
// fakes must implement.
type EKS interface {
	CreatePodIdentityAssociation(ctx context.Context, params *eks.CreatePodIdentityAssociationInput, optFns ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error)
	ListPodIdentityAssociations(ctx context.Context, params *eks.ListPodIdentityAssociationsInput, optFns ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error)
	DeletePodIdentityAssociation(ctx context.Context, params *eks.DeletePodIdentityAssociationInput, optFns ...func(*eks.Options)) (*eks.DeletePodIdentityAssociationOutput, error)
}

// Compile-time check that the real aws-sdk-go-v2 client satisfies our
// interface. If aws-sdk-go-v2 changes a method signature we'd hit it here at
// build time rather than at runtime.
var _ EKS = (*eks.Client)(nil)
