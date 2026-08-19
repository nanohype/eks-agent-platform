/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
)

// fakeS3 is a minimal in-memory awsclients.S3 holding one bucket-policy doc.
type fakeS3 struct {
	policy  *string // nil => GetBucketPolicy returns NoSuchBucketPolicy
	puts    []string
	deletes []string

	// Error-injection hooks (default nil = no error).
	getReturnsErr    error
	putReturnsErr    error
	deleteReturnsErr error
}

func (f *fakeS3) GetBucketPolicy(_ context.Context, _ *s3.GetBucketPolicyInput, _ ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	if f.getReturnsErr != nil {
		return nil, f.getReturnsErr
	}
	if f.policy == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchBucketPolicy", Message: "no policy set"}
	}
	return &s3.GetBucketPolicyOutput{Policy: f.policy}, nil
}

func (f *fakeS3) PutBucketPolicy(_ context.Context, params *s3.PutBucketPolicyInput, _ ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	if f.putReturnsErr != nil {
		return nil, f.putReturnsErr
	}
	doc := aws.ToString(params.Policy)

	// S3 rejects a policy whose Statement list is empty, and the fake must too — a fake
	// that accepts what the real API refuses cannot reproduce the bug this file exists
	// to prevent. The real error:
	//
	//	MalformedPolicy: Could not parse the policy: Statement is empty!
	var parsed map[string]any
	if err := json.Unmarshal([]byte(doc), &parsed); err == nil {
		if sts, ok := parsed["Statement"].([]any); ok && len(sts) == 0 {
			return nil, &smithy.GenericAPIError{
				Code:    "MalformedPolicy",
				Message: "Could not parse the policy: Statement is empty!",
			}
		}
	}

	f.policy = aws.String(doc) // persist so re-runs observe prior state
	f.puts = append(f.puts, doc)
	return &s3.PutBucketPolicyOutput{}, nil
}

func (f *fakeS3) DeleteBucketPolicy(_ context.Context, params *s3.DeleteBucketPolicyInput, _ ...func(*s3.Options)) (*s3.DeleteBucketPolicyOutput, error) {
	if f.deleteReturnsErr != nil {
		return nil, f.deleteReturnsErr
	}
	f.policy = nil
	f.deletes = append(f.deletes, aws.ToString(params.Bucket))
	return &s3.DeleteBucketPolicyOutput{}, nil
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

// statementBySid returns the one statement carrying sid, failing if it is absent
// or duplicated. The scope assertions below read fields off the statement rather
// than its Sid, so they need the statement itself.
func statementBySid(t *testing.T, raw, sid string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse policy json: %v", err)
	}
	stmts, _ := doc["Statement"].([]any)
	var found map[string]any
	for _, s := range stmts {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if m["Sid"] == sid {
			if found != nil {
				t.Fatalf("statement %q appears more than once", sid)
			}
			found = m
		}
	}
	if found == nil {
		t.Fatalf("statement %q not found in policy %s", sid, raw)
	}
	return found
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
	if countSid(sids, baselineDenyTLSSid) != 1 {
		t.Fatalf("the TLS-deny baseline must be seeded on an empty policy, got sids=%v", sids)
	}
}

// TestBaselineDenyInsecureTransport reads the statement the other tests only
// count.
//
// Everywhere else this baseline appears, the assertion is
// countSid(sids, baselineDenyTLSSid) == 1 — presence. Presence is the one
// property that cannot be wrong in an interesting way: the Sid is seeded from
// the same constant the count looks for, so it matches whatever the statement
// happens to say. An Effect of Allow on Principal "*" for s3:* would be counted
// as correctly present.
//
// Each field carries a distinct failure, and none of them error anywhere — the
// policy is structurally valid and S3 accepts it:
//
//	Effect        Allow instead of Deny grants the world s3:* on the bucket
//	Condition     absent denies *all* access including TLS, bricking the bucket;
//	              "true" instead of "false" denies only the traffic that was fine
//	Resource      the bucket ARN alone leaves every object unprotected, since
//	              object operations match arn:...:::bucket/* and not the bucket
func TestBaselineDenyInsecureTransport(t *testing.T) {
	const bucket = "artifacts"
	stmt := baselineDenyInsecureTransport(bucket)

	if got := stmt["Sid"]; got != baselineDenyTLSSid {
		t.Errorf("Sid = %v, want %v", got, baselineDenyTLSSid)
	}
	if got := stmt["Effect"]; got != "Deny" {
		t.Errorf("Effect = %v, want Deny — Allow here grants Principal * s3:* on the whole bucket", got)
	}
	if got := stmt["Principal"]; got != "*" {
		t.Errorf("Principal = %v, want * — the deny has to reach every caller to be a baseline", got)
	}
	if got := stmt["Action"]; got != "s3:*" {
		t.Errorf("Action = %v, want s3:* — a narrower action leaves the rest reachable over plaintext", got)
	}

	res, _ := stmt["Resource"].([]string)
	wantRes := []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"}
	if len(res) != len(wantRes) {
		t.Fatalf("Resource = %v, want both the bucket and its objects %v", res, wantRes)
	}
	for i := range wantRes {
		if res[i] != wantRes[i] {
			t.Errorf("Resource[%d] = %q, want %q — object operations match bucket/* and not the bucket ARN, "+
				"so dropping either half leaves that half in the clear", i, res[i], wantRes[i])
		}
	}

	cond, _ := stmt["Condition"].(map[string]any)
	b, _ := cond["Bool"].(map[string]any)
	if got := b["aws:SecureTransport"]; got != "false" {
		t.Errorf("Condition Bool aws:SecureTransport = %v, want \"false\" — this is what makes the statement "+
			"deny plaintext rather than deny everything (absent) or deny only TLS (\"true\")", got)
	}
}

// TestEnsureBucketPolicy_ScopesTenantAccessToItsOwnPrefix asserts the value that
// is the entire tenant boundary on the shared artifacts bucket.
//
// Nothing else separates tenants there. The bucket is one bucket for the whole
// cluster; tenant_baseline grants no s3: action at all, so a tenant role's only
// object access is the Allow this function writes, and SSE-KMS cannot
// discriminate because the bucket enables an S3 Bucket Key — the encryption
// context is the bucket ARN, identical for every tenant. If the Resource here
// widens to the bucket root, every tenant can read and overwrite every other
// tenant's fine-tuned weights, and no second control notices.
//
// The sibling tests in this file assert statement lifecycle — added, preserved,
// replaced, removed — all of which pass with the prefix collapsed to
// "tenants/". These two assertions are what make that mutation fail.
func TestEnsureBucketPolicy_ScopesTenantAccessToItsOwnPrefix(t *testing.T) {
	s := &fakeS3{}
	r := &PlatformReconciler{S3: s}
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}

	if err := r.ensureBucketPolicy(context.Background(), newPlatform("acme", "acme"), "role-arn", cfg); err != nil {
		t.Fatalf("ensureBucketPolicy: %v", err)
	}
	doc := s.puts[len(s.puts)-1]

	// Object access: exactly this tenant's prefix, never the bucket root.
	const wantResource = "arn:aws:s3:::artifacts/tenants/acme/*"
	if got := statementBySid(t, doc, "TenantAccess-acme")["Resource"]; got != wantResource {
		t.Errorf("object-access Resource: got %v want %q — anything broader than the "+
			"tenant's own prefix grants cross-tenant access to the shared bucket", got, wantResource)
	}

	// Listing: ListBucket is necessarily on the bucket ARN, so the prefix
	// condition is what keeps one tenant from enumerating another's object keys.
	list := statementBySid(t, doc, "TenantAccess-acme-List")
	cond, ok := list["Condition"].(map[string]any)
	if !ok {
		t.Fatalf("list statement has no Condition — ListBucket is granted on the bucket ARN, "+
			"so without an s3:prefix condition this enumerates every tenant's keys: %v", list)
	}
	like, _ := cond["StringLike"].(map[string]any)
	prefixes, _ := like["s3:prefix"].([]any)
	if len(prefixes) != 1 || prefixes[0] != "tenants/acme/*" {
		t.Errorf("list s3:prefix condition: got %v want [tenants/acme/*]", prefixes)
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
	// The TLS-deny baseline plus the two tenant statements — and none of them
	// accumulates across re-runs.
	if len(sids) != 3 {
		t.Fatalf("re-running must not accumulate statements, got sids=%v", sids)
	}
	if countSid(sids, baselineDenyTLSSid) != 1 {
		t.Fatalf("the TLS-deny baseline must be seeded exactly once, got sids=%v", sids)
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
	// No Put means the finalizer matched none of the seeded Sids and returned on
	// its !changed path — which is what a teardown that silently leaves a deleted
	// tenant's grant in place looks like from here. Asserted rather than indexed
	// into, so that failure reads as itself instead of as an index-out-of-range
	// panic from s.puts[-1].
	if len(s.puts) == 0 {
		t.Fatalf("teardown wrote no policy: the finalizer matched none of the seeded Sids %q/%q — "+
			"it and ensureBucketPolicy have drifted apart on how the tenant Sid is built",
			"TenantAccess-acme", "TenantAccess-acme-List")
	}
	sids := sidsOf(t, s.puts[len(s.puts)-1])
	if countSid(sids, "TenantAccess-acme") != 0 || countSid(sids, "TenantAccess-acme-List") != 0 {
		t.Errorf("own statements must be removed on teardown, sids=%v", sids)
	}
	if countSid(sids, "TenantAccess-other") != 1 {
		t.Errorf("a peer tenant's statement must survive teardown, sids=%v", sids)
	}
}

// Removing the LAST tenant's statements must DELETE the bucket policy, not write an
// empty one.
//
// This is the bug that wedged a real cluster. A single-tenant install has exactly one
// Platform, so its statements are the ONLY statements. Filtering them out left
// `Statement: []`, and S3 refuses it:
//
//	MalformedPolicy: Could not parse the policy: Statement is empty!
//
// The finalizer then retried forever. The Platform hung in Terminating, which pinned the
// agent-platform Application at Progressing — so ArgoCD's convergence gate could never
// pass — and `rackctl destroy` stalled on
// `platforms.platform.nanohype.dev did not finalize`.
//
// The correct way to say "this bucket has no policy" is to delete the policy.
func TestRemoveBucketPolicyStatements_DeletesPolicyWhenLastStatementRemoved(t *testing.T) {
	s := &fakeS3{policy: aws.String(`{
		"Version": "2012-10-17",
		"Statement": [
			{"Sid": "TenantAccess-acme", "Effect": "Allow", "Action": "s3:GetObject"},
			{"Sid": "TenantAccess-acme-List", "Effect": "Allow", "Action": "s3:ListBucket"}
		]
	}`)}
	r := &PlatformReconciler{S3: s}
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}

	if err := r.removeBucketPolicyStatements(context.Background(), newPlatform("acme", "acme"), cfg); err != nil {
		t.Fatalf("the finalizer must not fail when it removes the last statement — that is "+
			"what hangs the Platform in Terminating forever: %v", err)
	}
	if len(s.deletes) != 1 {
		t.Fatalf("want 1 DeleteBucketPolicy when no statements remain, got %d (puts=%d)",
			len(s.deletes), len(s.puts))
	}
	for _, p := range s.puts {
		if strings.Contains(p, `"Statement":[]`) || strings.Contains(p, `"Statement": []`) {
			t.Fatalf("wrote an empty-Statement policy, which S3 rejects: %s", p)
		}
	}
}

// A peer tenant's statements must survive: only DELETE the policy when nothing is left.
func TestRemoveBucketPolicyStatements_KeepsPolicyWhenAPeerTenantRemains(t *testing.T) {
	s := &fakeS3{policy: aws.String(`{
		"Version": "2012-10-17",
		"Statement": [
			{"Sid": "TenantAccess-acme", "Effect": "Allow", "Action": "s3:GetObject"},
			{"Sid": "TenantAccess-other", "Effect": "Allow", "Action": "s3:GetObject"}
		]
	}`)}
	r := &PlatformReconciler{S3: s}
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}

	if err := r.removeBucketPolicyStatements(context.Background(), newPlatform("acme", "acme"), cfg); err != nil {
		t.Fatalf("removeBucketPolicyStatements: %v", err)
	}
	if len(s.deletes) != 0 {
		t.Fatalf("must NOT delete the policy while a peer tenant still has statements — that "+
			"would silently revoke the other tenant's access; got %d deletes", len(s.deletes))
	}
	if len(s.puts) != 1 {
		t.Fatalf("want 1 PutBucketPolicy carrying the surviving statement, got %d", len(s.puts))
	}
	if got := sidsOf(t, s.puts[0]); len(got) != 1 || got[0] != "TenantAccess-other" {
		t.Fatalf("the peer tenant's statement must survive, got %v", got)
	}
}
