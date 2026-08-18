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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// PlatformAWSConfig is the slice of operatorconfig.Config the KMS + S3
// helpers need. Kept distinct from IAMConfig so the IAM path can run
// independently in dev (e.g. when the artifacts bucket isn't reachable from the
// operator role) without forcing the whole AWS surface to be online.
type PlatformAWSConfig struct {
	ArtifactsBucketName string
	Environment         string
}

// baselineDenyTLSSid is the Sid of the always-present statement that denies
// non-TLS access to the artifacts bucket. It is distinct from the per-tenant
// Sids so the per-tenant rewrite and the finalizer both preserve it.
const baselineDenyTLSSid = "DenyInsecureTransport"

// tenantAccessSid and tenantAccessListSid name the two statements the operator
// owns on the artifacts bucket policy for one Platform. Three call sites select
// statements by these names — the writer emitting them, the writer's own
// rewrite filter, and the finalizer's teardown — and a name is the only thing
// joining them, since a bucket policy is one shared document with no per-tenant
// structure to key on. Built here so the three cannot be composed differently,
// matching baselineDenyTLSSid above rather than recomposing the affix at each
// use.
func tenantAccessSid(p *platformv1alpha1.Platform) string {
	return "TenantAccess-" + p.Name
}

func tenantAccessListSid(p *platformv1alpha1.Platform) string {
	return tenantAccessSid(p) + "-List"
}

// baselineDenyInsecureTransport is the operator-owned in-transit-TLS guard for
// the artifacts bucket. terraform does not own this bucket's policy (the
// operator does), so the baseline lives here rather than in agent-iam.
func baselineDenyInsecureTransport(bucket string) map[string]any {
	return map[string]any{
		"Sid":       baselineDenyTLSSid,
		"Effect":    "Deny",
		"Principal": "*",
		"Action":    "s3:*",
		"Resource": []string{
			"arn:aws:s3:::" + bucket,
			"arn:aws:s3:::" + bucket + "/*",
		},
		"Condition": map[string]any{
			"Bool": map[string]any{"aws:SecureTransport": "false"},
		},
	}
}

// ensureBucketPolicy extends the artifacts bucket policy with a per-tenant
// statement granting r/w on tenants/<platform>/* to the tenant role ARN, and
// seeds a stable non-TLS deny baseline. Idempotent: rewrites the full policy
// each pass with a deterministic statement Sid per platform.
func (r *PlatformReconciler) ensureBucketPolicy(ctx context.Context, p *platformv1alpha1.Platform, roleARN string, cfg PlatformAWSConfig) error {
	if r.S3 == nil || cfg.ArtifactsBucketName == "" || roleARN == "" {
		return nil
	}
	bucket := cfg.ArtifactsBucketName
	sid := tenantAccessSid(p)
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
		"Sid":       tenantAccessListSid(p),
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
	// Drop any prior statements with this platform's Sid (idempotent rewrite),
	// noting whether the TLS-deny baseline is already present so it isn't added
	// twice.
	filtered := statements[:0]
	hasBaseline := false
	for _, s := range statements {
		if m, ok := s.(map[string]any); ok {
			existingSid, _ := m["Sid"].(string)
			if existingSid == sid || existingSid == tenantAccessListSid(p) {
				continue
			}
			if existingSid == baselineDenyTLSSid {
				hasBaseline = true
			}
		}
		filtered = append(filtered, s)
	}
	// Seed the non-TLS deny baseline if absent. Once seeded it survives every
	// per-tenant rewrite and the finalizer, so the bucket always denies non-TLS
	// access even after the last Platform is torn down.
	if !hasBaseline {
		filtered = append(filtered, baselineDenyInsecureTransport(bucket))
	}
	filtered = append(filtered, tenantStmt, listStmt)
	currentDoc["Statement"] = filtered
	if _, ok := currentDoc["Version"]; !ok {
		currentDoc["Version"] = "2012-10-17"
	}

	newBytes, err := json.Marshal(currentDoc)
	if err != nil {
		// currentDoc is JSON-native throughout (parsed policy + string/[]string
		// statements); marshal cannot fail. Defensive, unreachable, excluded.
		return fmt.Errorf("marshal bucket policy: %w", err) //coverage:ignore unreachable — JSON-native document
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
	sid := tenantAccessSid(p)
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
			if existingSid, _ := m["Sid"].(string); existingSid == sid || existingSid == tenantAccessListSid(p) {
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
		// currentDoc is JSON-native throughout (parsed policy + string/[]string
		// statements); marshal cannot fail. Defensive, unreachable, excluded.
		return fmt.Errorf("marshal bucket policy: %w", err) //coverage:ignore unreachable — JSON-native document
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
