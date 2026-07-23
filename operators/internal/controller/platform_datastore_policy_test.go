/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func testScope() arnScope {
	return arnScope{Partition: "aws", Region: "us-west-2", AccountID: "123456789012"}
}

func platformWithDatastores(name string, ds ...platformv1alpha1.DatastoreSpec) *platformv1alpha1.Platform { //nolint:unparam // policy-generation unit tests use a fixed platform token
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       platformv1alpha1.PlatformSpec{Datastores: ds},
	}
}

func findStmt(stmts []policyStatement, sid string) *policyStatement {
	for i := range stmts {
		if stmts[i].Sid == sid {
			return &stmts[i]
		}
	}
	return nil
}

func hasResource(s *policyStatement, want string) bool {
	for _, r := range s.Resource {
		if r == want {
			return true
		}
	}
	return false
}

// TestDatastorePolicy_ScopesEachKind proves every datastore kind is granted its
// minimal actions scoped to the tenant-substrate naming convention
// (<env>-<platform>-<datastore>, S3 account-qualified), cache contributes no
// statement, and no Resource is a bare wildcard.
func TestDatastorePolicy_ScopesEachKind(t *testing.T) {
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "kv", Kind: platformv1alpha1.DatastoreKeyValue},
		platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore},
		platformv1alpha1.DatastoreSpec{Name: "q", Kind: platformv1alpha1.DatastoreQueue},
		platformv1alpha1.DatastoreSpec{Name: "ca", Kind: platformv1alpha1.DatastoreCache},
		platformv1alpha1.DatastoreSpec{Name: "st", Kind: platformv1alpha1.DatastoreStream},
	)
	stmts := datastorePolicyStatements(p, "development", testScope())

	// S3: bucket + object statements, account-qualified name.
	if s := findStmt(stmts, "s3bucketobj"); s == nil || !hasResource(s, "arn:aws:s3:::development-myplat-obj-123456789012") {
		t.Errorf("s3 bucket statement missing or misscoped: %+v", s)
	}
	if s := findStmt(stmts, "s3objectobj"); s == nil || !hasResource(s, "arn:aws:s3:::development-myplat-obj-123456789012/*") {
		t.Errorf("s3 object statement missing or misscoped: %+v", s)
	}

	// DynamoDB: table + index.
	if s := findStmt(stmts, "dynamodbkv"); s == nil ||
		!hasResource(s, "arn:aws:dynamodb:us-west-2:123456789012:table/development-myplat-kv") ||
		!hasResource(s, "arn:aws:dynamodb:us-west-2:123456789012:table/development-myplat-kv/index/*") {
		t.Errorf("dynamodb statement missing or misscoped: %+v", s)
	}

	// SQS: prefix wildcard covers the queue, its .fifo, and its DLQ.
	if s := findStmt(stmts, "sqsq"); s == nil || !hasResource(s, "arn:aws:sqs:us-west-2:123456789012:development-myplat-q*") {
		t.Errorf("sqs statement missing or misscoped: %+v", s)
	}

	// MSK: cluster/topic/group ARNs under the tenant's own cluster name.
	if s := findStmt(stmts, "mskst"); s == nil ||
		!hasResource(s, "arn:aws:kafka:us-west-2:123456789012:cluster/development-myplat-st/*") ||
		!hasResource(s, "arn:aws:kafka:us-west-2:123456789012:topic/development-myplat-st/*") ||
		!hasResource(s, "arn:aws:kafka:us-west-2:123456789012:group/development-myplat-st/*") {
		t.Errorf("msk statement missing or misscoped: %+v", s)
	}

	// relational: one shared secret grant scoped to the RDS-managed prefix.
	if s := findStmt(stmts, "relationalSecrets"); s == nil ||
		!hasResource(s, "arn:aws:secretsmanager:us-west-2:123456789012:secret:rds!cluster-*") {
		t.Errorf("relational secret statement missing or misscoped: %+v", s)
	}

	// cache contributes no statement — nothing named for "ca".
	for _, s := range stmts {
		if strings.Contains(s.Sid, "ca") && s.Sid != "relationalSecrets" {
			t.Errorf("cache datastore should produce no statement, found: %s", s.Sid)
		}
	}

	// no bare-wildcard Resource, and every statement is an Allow.
	for _, s := range stmts {
		if s.Effect != "Allow" {
			t.Errorf("statement %s must be Allow, got %s", s.Sid, s.Effect)
		}
		for _, r := range s.Resource {
			if r == "*" {
				t.Errorf("statement %s has a bare-wildcard Resource", s.Sid)
			}
		}
	}
}

// TestDatastorePolicy_EmptyWhenNoIAMDatastores proves a Platform with no
// datastores — or only a cache, which needs no IAM — produces no policy, so the
// reconciler removes the inline policy rather than writing an empty one.
func TestDatastorePolicy_EmptyWhenNoIAMDatastores(t *testing.T) {
	none := datastorePolicyStatements(platformWithDatastores("myplat"), "development", testScope())
	if len(none) != 0 {
		t.Errorf("no datastores must yield no statements, got %d", len(none))
	}
	doc, err := datastorePolicyDoc(none)
	if err != nil {
		t.Fatalf("datastorePolicyDoc: %v", err)
	}
	if doc != "" {
		t.Errorf("empty statements must yield an empty document, got %q", doc)
	}

	cacheOnly := datastorePolicyStatements(
		platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "ca", Kind: platformv1alpha1.DatastoreCache}),
		"development", testScope(),
	)
	if len(cacheOnly) != 0 {
		t.Errorf("a cache-only Platform needs no IAM statement, got %d", len(cacheOnly))
	}
}

// TestDatastorePolicy_RelationalSecretDeduped proves multiple relational
// datastores share the single RDS-managed-secret grant rather than emitting a
// redundant statement each.
func TestDatastorePolicy_RelationalSecretDeduped(t *testing.T) {
	p := platformWithDatastores("myplat",
		platformv1alpha1.DatastoreSpec{Name: "a", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "b", Kind: platformv1alpha1.DatastoreRelational},
	)
	stmts := datastorePolicyStatements(p, "development", testScope())
	secretCount := 0
	for _, s := range stmts {
		if s.Sid == "relationalSecrets" {
			secretCount++
		}
	}
	if secretCount != 1 {
		t.Errorf("two relational datastores must share one secret grant, got %d", secretCount)
	}
}

// TestEnsureDatastorePolicy_WritesAndConverges proves the reconcile writes the
// inline datastore-access policy once and then no-ops on a converged re-run.
func TestEnsureDatastorePolicy_WritesAndConverges(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{Environment: "development", Region: "us-west-2"}
	p := platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore})

	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", p, cfg); err != nil {
		t.Fatalf("ensureDatastorePolicy: %v", err)
	}
	if len(f.putInlineCalls) != 1 {
		t.Fatalf("PutRolePolicy calls: got %d want 1", len(f.putInlineCalls))
	}
	if got := *f.putInlineCalls[0].PolicyName; got != datastorePolicyName {
		t.Errorf("policy name: got %q want %q", got, datastorePolicyName)
	}
	if doc := *f.putInlineCalls[0].PolicyDocument; !strings.Contains(doc, "development-myplat-obj-123456789012") {
		t.Errorf("policy document not scoped to the datastore's bucket: %s", doc)
	}

	// Converged re-run must not write again.
	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", p, cfg); err != nil {
		t.Fatalf("ensureDatastorePolicy re-run: %v", err)
	}
	if len(f.putInlineCalls) != 1 {
		t.Errorf("converged re-run must not re-write: got %d PutRolePolicy calls", len(f.putInlineCalls))
	}
}

// TestEnsureDatastorePolicy_RemovesWhenEmpty proves a Platform with no
// IAM-needing datastore has the inline policy removed, so a cleared declaration
// leaves no stale grant.
func TestEnsureDatastorePolicy_RemovesWhenEmpty(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{Environment: "development", Region: "us-west-2"}

	if err := r.ensureDatastorePolicy(context.Background(), "test-role", "arn:aws:iam::123456789012:role/test-role", platformWithDatastores("myplat"), cfg); err != nil {
		t.Fatalf("ensureDatastorePolicy: %v", err)
	}
	if len(f.putInlineCalls) != 0 {
		t.Errorf("no datastores must write no policy: got %d PutRolePolicy calls", len(f.putInlineCalls))
	}
	if len(f.deleteInlineCalls) != 1 || *f.deleteInlineCalls[0].PolicyName != datastorePolicyName {
		t.Errorf("no datastores must delete the datastore-access policy: got %+v", f.deleteInlineCalls)
	}
}

// TestEnsureDatastorePolicy_NilIAM proves the reconcile no-ops when the operator
// has no IAM client (the guard the fake tests never exercise).
func TestEnsureDatastorePolicy_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if err := r.ensureDatastorePolicy(context.Background(), "role", "arn:aws:iam::123456789012:role/role",
		platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}),
		IAMConfig{}); err != nil {
		t.Fatalf("nil IAM must no-op: %v", err)
	}
}

// TestEnsureDatastorePolicy_DeleteErrorPropagates proves a non-NotFound
// DeleteRolePolicy failure on the removal path surfaces rather than being
// swallowed.
func TestEnsureDatastorePolicy_DeleteErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("role", "arn:aws:iam::123456789012:role/role")
	f.deleteInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	// No datastores -> the reconcile deletes the inline policy; the injected
	// error must propagate.
	if err := r.ensureDatastorePolicy(context.Background(), "role", "arn:aws:iam::123456789012:role/role",
		platformWithDatastores("myplat"), IAMConfig{Environment: "development", Region: "us-west-2"}); err == nil {
		t.Fatalf("expected the DeleteRolePolicy error to propagate")
	}
}

// TestEnsureDatastorePolicy_PutErrorPropagates proves a PutRolePolicy failure on
// the write path surfaces.
func TestEnsureDatastorePolicy_PutErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("role", "arn:aws:iam::123456789012:role/role")
	f.putInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureDatastorePolicy(context.Background(), "role", "arn:aws:iam::123456789012:role/role",
		platformWithDatastores("myplat", platformv1alpha1.DatastoreSpec{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}),
		IAMConfig{Environment: "development", Region: "us-west-2"}); err == nil {
		t.Fatalf("expected the PutRolePolicy error to propagate")
	}
}

func datastoreErrCfg() IAMConfig {
	return IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/baseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
		Region:                  "us-west-2",
	}
}

// TestEnsureIamRole_DatastorePolicyError_CreatePath proves ensureIamRole
// propagates a datastore-policy failure on the create path (the model-scoping
// reconcile still succeeds — only datastore-access errs).
func TestEnsureIamRole_DatastorePolicyError_CreatePath(t *testing.T) {
	f := newFakeIAM()
	f.getInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}}

	if _, err := r.ensureIamRole(context.Background(), p, datastoreErrCfg()); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the datastore-policy error on the create path")
	}
}

// TestEnsureIamRole_DatastorePolicyError_ExistingRolePath proves the same on the
// existing-role path (role already present, not suspended).
func TestEnsureIamRole_DatastorePolicyError_ExistingRolePath(t *testing.T) {
	f := newFakeIAM()
	cfg := datastoreErrCfg()
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore}}

	roleName := tenantRoleName(cfg.ClusterName, p)
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.getInlineReturnsErr = map[string]error{datastorePolicyName: errors.New("boom")}

	if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the datastore-policy error on the existing-role path")
	}
}
