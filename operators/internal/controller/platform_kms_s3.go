/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// PlatformAWSConfig is the slice of operatorconfig.Config the KMS + S3
// helpers need. Kept distinct from IAMConfig so the IAM path can run
// independently in dev (e.g. when cmk-data isn't reachable from the
// operator role) without forcing the whole AWS surface to be online.
type PlatformAWSConfig struct {
	DataKMSKeyARN       string
	ArtifactsBucketName string
	Environment         string
}

// ensureKmsGrant creates a KMS grant on cmk-data scoped to the tenant
// IRSA role, with EncryptionContext = {PlatformId: <name>}. Idempotent:
// ListGrants first; CreateGrant only when no grant for this role+context
// already exists.
//
// The EncryptionContext is the load-bearing isolation primitive — tenant
// role A's grant doesn't let it decrypt tenant B's data because B's
// data is encrypted under EncryptionContext={PlatformId:B}.
func (r *PlatformReconciler) ensureKmsGrant(ctx context.Context, p *platformv1alpha1.Platform, roleARN string, cfg PlatformAWSConfig) error {
	if r.KMS == nil || cfg.DataKMSKeyARN == "" || roleARN == "" {
		return nil
	}
	grantName := "tenant-" + p.Name

	// Idempotency: enumerate existing grants on the key and short-circuit
	// if a grant for this name already exists. KMS doesn't have an
	// upsert-by-name API; the alternative is a stable Name tag that
	// ListGrants exposes.
	var marker *string
	for {
		out, err := r.KMS.ListGrants(ctx, &kms.ListGrantsInput{
			KeyId:  aws.String(cfg.DataKMSKeyARN),
			Marker: marker,
		})
		if err != nil {
			return fmt.Errorf("kms ListGrants: %w", err)
		}
		for _, g := range out.Grants {
			if g.Name != nil && *g.Name == grantName {
				return nil
			}
		}
		if !out.Truncated || out.NextMarker == nil {
			break
		}
		marker = out.NextMarker
	}

	_, err := r.KMS.CreateGrant(ctx, &kms.CreateGrantInput{
		KeyId:            aws.String(cfg.DataKMSKeyARN),
		GranteePrincipal: aws.String(roleARN),
		Name:             aws.String(grantName),
		Operations: []kmstypes.GrantOperation{
			kmstypes.GrantOperationDecrypt,
			kmstypes.GrantOperationEncrypt,
			kmstypes.GrantOperationGenerateDataKey,
			kmstypes.GrantOperationGenerateDataKeyWithoutPlaintext,
			kmstypes.GrantOperationDescribeKey,
		},
		Constraints: &kmstypes.GrantConstraints{
			// Tenant data must be encrypted with this exact context.
			// Cross-tenant decrypt is prevented by the constraint
			// mismatch (operator can't grant a freeform key/ctx pair —
			// see kms:GrantIsForAWSResource condition in agent-iam).
			EncryptionContextEquals: map[string]string{
				"PlatformId": p.Name,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("kms CreateGrant: %w", err)
	}
	return nil
}

// revokeKmsGrant is the finalizer counterpart: enumerate grants on
// cmk-data and revoke any whose Name matches our tenant grant.
func (r *PlatformReconciler) revokeKmsGrant(ctx context.Context, p *platformv1alpha1.Platform, cfg PlatformAWSConfig) error {
	if r.KMS == nil || cfg.DataKMSKeyARN == "" {
		return nil
	}
	grantName := "tenant-" + p.Name
	var marker *string
	for {
		out, err := r.KMS.ListGrants(ctx, &kms.ListGrantsInput{
			KeyId:  aws.String(cfg.DataKMSKeyARN),
			Marker: marker,
		})
		if err != nil {
			return fmt.Errorf("kms ListGrants (revoke path): %w", err)
		}
		for _, g := range out.Grants {
			if g.Name != nil && *g.Name == grantName && g.GrantId != nil {
				_, err := r.KMS.RevokeGrant(ctx, &kms.RevokeGrantInput{
					KeyId:   aws.String(cfg.DataKMSKeyARN),
					GrantId: g.GrantId,
				})
				if err != nil && !isAPIErrorCode(err, "NotFoundException") {
					return fmt.Errorf("kms RevokeGrant %s: %w", *g.GrantId, err)
				}
			}
		}
		if !out.Truncated || out.NextMarker == nil {
			break
		}
		marker = out.NextMarker
	}
	return nil
}

// ensureBucketPolicy extends the artifacts bucket policy with a per-tenant
// statement granting r/w on tenants/<platform>/* to the tenant role ARN.
// Idempotent: rewrites the full policy each pass with a deterministic
// statement Sid per platform.
func (r *PlatformReconciler) ensureBucketPolicy(ctx context.Context, p *platformv1alpha1.Platform, roleARN string, cfg PlatformAWSConfig) error {
	if r.S3 == nil || cfg.ArtifactsBucketName == "" || roleARN == "" {
		return nil
	}
	bucket := cfg.ArtifactsBucketName
	sid := "TenantAccess-" + p.Name
	prefix := "tenants/" + p.Name + "/"
	tenantStmt := map[string]any{
		"Sid":       sid,
		"Effect":    "Allow",
		"Principal": map[string]any{"AWS": roleARN},
		"Action": []string{
			"s3:GetObject",
			"s3:PutObject",
			"s3:DeleteObject",
			"s3:AbortMultipartUpload",
			"s3:ListMultipartUploadParts",
		},
		"Resource": "arn:aws:s3:::" + bucket + "/" + prefix + "*",
	}
	listStmt := map[string]any{
		"Sid":       sid + "-List",
		"Effect":    "Allow",
		"Principal": map[string]any{"AWS": roleARN},
		"Action":    "s3:ListBucket",
		"Resource":  "arn:aws:s3:::" + bucket,
		"Condition": map[string]any{
			"StringLike": map[string]any{
				"s3:prefix": []string{prefix + "*"},
			},
		},
	}

	// Serialize the shared-bucket-policy read-modify-write so concurrent
	// reconciles can't interleave Get→mutate→Put and drop a peer tenant's
	// statement (see PlatformReconciler.bucketPolicyMu).
	r.bucketPolicyMu.Lock()
	defer r.bucketPolicyMu.Unlock()

	currentDoc, err := r.fetchBucketPolicy(ctx, bucket)
	if err != nil {
		return err
	}
	statements, _ := currentDoc["Statement"].([]any)
	// Drop any prior statements with the same Sid (idempotent rewrite).
	filtered := statements[:0]
	for _, s := range statements {
		if m, ok := s.(map[string]any); ok {
			if existingSid, _ := m["Sid"].(string); existingSid == sid || existingSid == sid+"-List" {
				continue
			}
		}
		filtered = append(filtered, s)
	}
	filtered = append(filtered, tenantStmt, listStmt)
	currentDoc["Statement"] = filtered
	if _, ok := currentDoc["Version"]; !ok {
		currentDoc["Version"] = "2012-10-17"
	}

	newBytes, err := json.Marshal(currentDoc)
	if err != nil {
		return fmt.Errorf("marshal bucket policy: %w", err)
	}
	if _, err := r.S3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(string(newBytes)),
	}); err != nil {
		return fmt.Errorf("s3 PutBucketPolicy: %w", err)
	}
	return nil
}

// removeBucketPolicyStatements is the finalizer counterpart: drops the
// tenant statements from the bucket policy. Idempotent.
func (r *PlatformReconciler) removeBucketPolicyStatements(ctx context.Context, p *platformv1alpha1.Platform, cfg PlatformAWSConfig) error {
	if r.S3 == nil || cfg.ArtifactsBucketName == "" {
		return nil
	}
	bucket := cfg.ArtifactsBucketName
	sid := "TenantAccess-" + p.Name
	// Same shared-document serialization as ensureBucketPolicy: a finalizer
	// teardown must not interleave with a peer tenant's reconcile write.
	r.bucketPolicyMu.Lock()
	defer r.bucketPolicyMu.Unlock()
	currentDoc, err := r.fetchBucketPolicy(ctx, bucket)
	if err != nil {
		return err
	}
	statements, _ := currentDoc["Statement"].([]any)
	if len(statements) == 0 {
		return nil
	}
	filtered := statements[:0]
	changed := false
	for _, s := range statements {
		if m, ok := s.(map[string]any); ok {
			if existingSid, _ := m["Sid"].(string); existingSid == sid || existingSid == sid+"-List" {
				changed = true
				continue
			}
		}
		filtered = append(filtered, s)
	}
	if !changed {
		return nil
	}
	// A bucket policy with no statements is not a valid bucket policy. S3 rejects it:
	//
	//	MalformedPolicy: Could not parse the policy: Statement is empty!
	//
	// When this Platform owns the only statements — which is the ordinary case, since a
	// single-tenant cluster has exactly one — filtering them out leaves an empty list,
	// and PutBucketPolicy fails. The finalizer then retries forever, the Platform hangs
	// in Terminating, and everything downstream wedges with it: the agent-platform
	// Application never leaves Progressing (so the convergence gate can never pass), and
	// `rackctl destroy` stalls on `platforms.platform.nanohype.dev did not finalize`.
	//
	// The correct way to express "this bucket has no policy" is to DELETE the policy, not
	// to write an empty one.
	if len(filtered) == 0 {
		if _, err := r.S3.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{
			Bucket: aws.String(bucket),
		}); err != nil {
			return fmt.Errorf("s3 DeleteBucketPolicy (finalizer, last statement removed): %w", err)
		}
		return nil
	}

	currentDoc["Statement"] = filtered
	newBytes, err := json.Marshal(currentDoc)
	if err != nil {
		return fmt.Errorf("marshal bucket policy: %w", err)
	}
	if _, err := r.S3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(string(newBytes)),
	}); err != nil {
		return fmt.Errorf("s3 PutBucketPolicy (finalizer): %w", err)
	}
	return nil
}

// fetchBucketPolicy returns the parsed policy document, or an empty doc
// if no policy is set on the bucket.
func (r *PlatformReconciler) fetchBucketPolicy(ctx context.Context, bucket string) (map[string]any, error) {
	out, err := r.S3.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil {
		if isAPIErrorCode(err, "NoSuchBucketPolicy") {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("s3 GetBucketPolicy: %w", err)
	}
	var doc map[string]any
	if out.Policy != nil {
		if err := json.Unmarshal([]byte(*out.Policy), &doc); err != nil {
			return nil, fmt.Errorf("parse bucket policy JSON: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// isAPIErrorCode is a smithy.APIError predicate by code (e.g.
// "NoSuchEntity", "NoSuchBucketPolicy", "NotFoundException").
func isAPIErrorCode(err error, code string) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == code
	}
	return false
}
