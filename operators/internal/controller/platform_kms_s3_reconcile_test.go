/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
)

// fakeKMS is a minimal in-memory awsclients.KMS. The interface is exactly the
// three methods below, so there is nothing left to panic-guard.
type fakeKMS struct {
	grants       []kmstypes.GrantListEntry
	created      []kms.CreateGrantInput
	revoked      []string
	pageBoundary int // if > 0, paginate ListGrants at this size
}

func (f *fakeKMS) ListGrants(_ context.Context, params *kms.ListGrantsInput, _ ...func(*kms.Options)) (*kms.ListGrantsOutput, error) {
	if f.pageBoundary > 0 && len(f.grants) > f.pageBoundary {
		if params.Marker == nil {
			return &kms.ListGrantsOutput{Grants: f.grants[:f.pageBoundary], Truncated: true, NextMarker: aws.String("page-2")}, nil
		}
		return &kms.ListGrantsOutput{Grants: f.grants[f.pageBoundary:]}, nil
	}
	return &kms.ListGrantsOutput{Grants: f.grants}, nil
}

func (f *fakeKMS) CreateGrant(_ context.Context, params *kms.CreateGrantInput, _ ...func(*kms.Options)) (*kms.CreateGrantOutput, error) {
	f.created = append(f.created, *params)
	id := "grant-" + aws.ToString(params.Name)
	f.grants = append(f.grants, kmstypes.GrantListEntry{Name: params.Name, GrantId: aws.String(id)})
	return &kms.CreateGrantOutput{GrantId: aws.String(id)}, nil
}

func (f *fakeKMS) RevokeGrant(_ context.Context, params *kms.RevokeGrantInput, _ ...func(*kms.Options)) (*kms.RevokeGrantOutput, error) {
	f.revoked = append(f.revoked, aws.ToString(params.GrantId))
	return &kms.RevokeGrantOutput{}, nil
}

// fakeS3 is a minimal in-memory awsclients.S3 holding one bucket-policy doc.
type fakeS3 struct {
	policy *string // nil => GetBucketPolicy returns NoSuchBucketPolicy
	puts   []string
}

func (f *fakeS3) GetBucketPolicy(_ context.Context, _ *s3.GetBucketPolicyInput, _ ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	if f.policy == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchBucketPolicy", Message: "no policy set"}
	}
	return &s3.GetBucketPolicyOutput{Policy: f.policy}, nil
}

func (f *fakeS3) PutBucketPolicy(_ context.Context, params *s3.PutBucketPolicyInput, _ ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	doc := aws.ToString(params.Policy)
	f.policy = aws.String(doc) // persist so re-runs observe prior state
	f.puts = append(f.puts, doc)
	return &s3.PutBucketPolicyOutput{}, nil
}

func sidsOf(t *testing.T, raw string) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse policy json: %v", err)
	}
	stmts, _ := doc["Statement"].([]any)
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if m, ok := s.(map[string]any); ok {
			if sid, ok := m["Sid"].(string); ok {
				out = append(out, sid)
			}
		}
	}
	return out
}

func countSid(sids []string, want string) int {
	n := 0
	for _, s := range sids {
		if s == want {
			n++
		}
	}
	return n
}

func TestEnsureKmsGrant_CreatesTenantScopedGrant(t *testing.T) {
	k := &fakeKMS{}
	r := &PlatformReconciler{KMS: k}
	cfg := PlatformAWSConfig{DataKMSKeyARN: "arn:aws:kms:us-west-2:123456789012:key/abc"}
	role := "arn:aws:iam::123456789012:role/tenant-acme"

	if err := r.ensureKmsGrant(context.Background(), newPlatform("acme", "acme"), role, cfg); err != nil {
		t.Fatalf("ensureKmsGrant: %v", err)
	}
	if len(k.created) != 1 {
		t.Fatalf("want exactly 1 CreateGrant, got %d", len(k.created))
	}
	got := k.created[0]
	if aws.ToString(got.GranteePrincipal) != role {
		t.Errorf("grantee = %q, want %q", aws.ToString(got.GranteePrincipal), role)
	}
	if aws.ToString(got.Name) != "tenant-acme" {
		t.Errorf("grant name = %q, want tenant-acme", aws.ToString(got.Name))
	}
	// The EncryptionContext is the load-bearing cross-tenant isolation primitive.
	if got.Constraints == nil || got.Constraints.EncryptionContextEquals["PlatformId"] != "acme" {
		t.Fatalf("EncryptionContextEquals[PlatformId] != acme: %+v", got.Constraints)
	}
}

func TestEnsureKmsGrant_IdempotentWhenGrantExists(t *testing.T) {
	k := &fakeKMS{grants: []kmstypes.GrantListEntry{{Name: aws.String("tenant-acme"), GrantId: aws.String("g1")}}}
	r := &PlatformReconciler{KMS: k}
	cfg := PlatformAWSConfig{DataKMSKeyARN: "arn:aws:kms:us-west-2:123:key/abc"}

	if err := r.ensureKmsGrant(context.Background(), newPlatform("acme", "acme"), "role", cfg); err != nil {
		t.Fatalf("ensureKmsGrant: %v", err)
	}
	if len(k.created) != 0 {
		t.Fatalf("an existing grant must short-circuit CreateGrant, got %d creates", len(k.created))
	}
}

func TestEnsureKmsGrant_FindsGrantOnSecondPage(t *testing.T) {
	k := &fakeKMS{
		grants: []kmstypes.GrantListEntry{
			{Name: aws.String("tenant-other"), GrantId: aws.String("g0")},
			{Name: aws.String("tenant-acme"), GrantId: aws.String("g1")},
		},
		pageBoundary: 1,
	}
	r := &PlatformReconciler{KMS: k}
	cfg := PlatformAWSConfig{DataKMSKeyARN: "arn:aws:kms:us-west-2:123:key/abc"}

	if err := r.ensureKmsGrant(context.Background(), newPlatform("acme", "acme"), "role", cfg); err != nil {
		t.Fatalf("ensureKmsGrant: %v", err)
	}
	if len(k.created) != 0 {
		t.Fatalf("a grant on page 2 must be found before create, got %d creates", len(k.created))
	}
}

func TestEnsureBucketPolicy_AddsTenantStatementsToEmptyPolicy(t *testing.T) {
	s := &fakeS3{} // nil => NoSuchBucketPolicy => starts from an empty doc
	r := &PlatformReconciler{S3: s}
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}

	if err := r.ensureBucketPolicy(context.Background(), newPlatform("acme", "acme"), "role-arn", cfg); err != nil {
		t.Fatalf("ensureBucketPolicy: %v", err)
	}
	if len(s.puts) != 1 {
		t.Fatalf("want 1 PutBucketPolicy, got %d", len(s.puts))
	}
	sids := sidsOf(t, s.puts[len(s.puts)-1])
	if countSid(sids, "TenantAccess-acme") != 1 || countSid(sids, "TenantAccess-acme-List") != 1 {
		t.Fatalf("expected both tenant statements exactly once, got sids=%v", sids)
	}
}

func TestEnsureBucketPolicy_PreservesForeignReplacesOwn(t *testing.T) {
	seed := `{"Version":"2012-10-17","Statement":[` +
		`{"Sid":"TenantAccess-other","Effect":"Allow","Principal":{"AWS":"other-role"}},` +
		`{"Sid":"TenantAccess-acme","Effect":"Allow","Resource":"stale"}]}`
	s := &fakeS3{policy: aws.String(seed)}
	r := &PlatformReconciler{S3: s}
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}

	if err := r.ensureBucketPolicy(context.Background(), newPlatform("acme", "acme"), "role-arn", cfg); err != nil {
		t.Fatalf("ensureBucketPolicy: %v", err)
	}
	sids := sidsOf(t, s.puts[len(s.puts)-1])
	if countSid(sids, "TenantAccess-other") != 1 {
		t.Errorf("a peer tenant's statement must survive the merge, sids=%v", sids)
	}
	if countSid(sids, "TenantAccess-acme") != 1 {
		t.Errorf("own statement must be replaced, not duplicated, sids=%v", sids)
	}
	if countSid(sids, "TenantAccess-acme-List") != 1 {
		t.Errorf("own list statement must be present once, sids=%v", sids)
	}
}

func TestEnsureBucketPolicy_IdempotentAcrossRuns(t *testing.T) {
	s := &fakeS3{}
	r := &PlatformReconciler{S3: s}
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}
	p := newPlatform("acme", "acme")

	for i := 0; i < 3; i++ {
		if err := r.ensureBucketPolicy(context.Background(), p, "role-arn", cfg); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	sids := sidsOf(t, s.puts[len(s.puts)-1])
	if len(sids) != 2 {
		t.Fatalf("re-running must not accumulate statements, got sids=%v", sids)
	}
}

func TestRemoveBucketPolicyStatements_DropsOwnKeepsForeign(t *testing.T) {
	seed := `{"Version":"2012-10-17","Statement":[` +
		`{"Sid":"TenantAccess-other","Effect":"Allow"},` +
		`{"Sid":"TenantAccess-acme","Effect":"Allow"},` +
		`{"Sid":"TenantAccess-acme-List","Effect":"Allow"}]}`
	s := &fakeS3{policy: aws.String(seed)}
	r := &PlatformReconciler{S3: s}
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}

	if err := r.removeBucketPolicyStatements(context.Background(), newPlatform("acme", "acme"), cfg); err != nil {
		t.Fatalf("removeBucketPolicyStatements: %v", err)
	}
	sids := sidsOf(t, s.puts[len(s.puts)-1])
	if countSid(sids, "TenantAccess-acme") != 0 || countSid(sids, "TenantAccess-acme-List") != 0 {
		t.Errorf("own statements must be removed on teardown, sids=%v", sids)
	}
	if countSid(sids, "TenantAccess-other") != 1 {
		t.Errorf("a peer tenant's statement must survive teardown, sids=%v", sids)
	}
}
