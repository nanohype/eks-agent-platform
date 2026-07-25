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
)

// A real CMK ARN: the key id is AWS-generated, which is why the ARN has to be
// published by the substrate rather than composed from the naming convention.
const testTenantKeyARN = "arn:aws:kms:us-west-2:123456789012:key/9a1f2c3d-4b5e-6789-a0bc-de1234567890"

// TestTenantKeyPolicy_ScopesToTheTenantsOwnKey pins the property the whole key
// exists to create. A pattern here would reach every tenant's key and undo the
// isolation the per-tenant CMK was minted for.
func TestTenantKeyPolicy_ScopesToTheTenantsOwnKey(t *testing.T) {
	stmts := tenantKeyPolicyStatements(testTenantKeyARN)

	if len(stmts) != 1 {
		t.Fatalf("expected exactly one statement, got %d", len(stmts))
	}
	s := stmts[0]
	if s.Effect != "Allow" || s.Sid != "tenantKeyEnvelope" {
		t.Errorf("unexpected statement shape: %+v", s)
	}
	if len(s.Resource) != 1 || s.Resource[0] != testTenantKeyARN {
		t.Errorf("the grant must name exactly the published key ARN, got %v", s.Resource)
	}
	for _, r := range s.Resource {
		if strings.Contains(r, "*") {
			t.Errorf("a pattern here spans tenants: %s", r)
		}
	}
}

// The action set is the envelope-encryption minimum. Encrypt is deliberately
// absent — the pattern is GenerateDataKey then Decrypt — and no key-management
// verb belongs on a tenant role.
func TestTenantKeyPolicy_GrantsOnlyEnvelopeActions(t *testing.T) {
	s := tenantKeyPolicyStatements(testTenantKeyARN)[0]

	want := map[string]bool{"kms:GenerateDataKey": true, "kms:Decrypt": true, "kms:DescribeKey": true}
	for _, a := range s.Action {
		if !want[a] {
			t.Errorf("unexpected action on a tenant role: %s", a)
		}
		delete(want, a)
	}
	if len(want) != 0 {
		t.Errorf("missing envelope actions: %v", want)
	}
	for _, a := range s.Action {
		if strings.HasPrefix(a, "kms:Create") || strings.HasPrefix(a, "kms:Put") ||
			strings.HasPrefix(a, "kms:Schedule") || a == "kms:Encrypt" {
			t.Errorf("tenant roles use the key, they do not manage it: %s", a)
		}
	}
}

// Fail-closed: no published ARN means no grant, never a broader one.
func TestTenantKeyPolicy_UnpublishedKeyGrantsNothing(t *testing.T) {
	if stmts := tenantKeyPolicyStatements(""); len(stmts) != 0 {
		t.Errorf("an unpublished key must produce no statement, got %+v", stmts)
	}
	doc, err := datastorePolicyDoc(tenantKeyPolicyStatements(""))
	if err != nil {
		t.Fatalf("datastorePolicyDoc: %v", err)
	}
	if doc != "" {
		t.Errorf("no statements must yield an empty document so the caller removes the policy, got %q", doc)
	}
}

func TestTenantKeyParamPath(t *testing.T) {
	got := tenantKeyParamPath("development-platform", "myplat")
	want := "/eks-agent-platform/development-platform/tenant-substrate/myplat/kms_key_arn"
	if got != want {
		t.Errorf("SSM path drifted from what tenant-substrate publishes:\n got %s\nwant %s", got, want)
	}
}

func TestResolveTenantKeyARN(t *testing.T) {
	const cluster = "development-platform"
	p := platformWithDatastores("myplat")
	path := tenantKeyParamPath(cluster, "myplat")

	t.Run("published key resolves", func(t *testing.T) {
		r := &PlatformReconciler{SSM: &stubSSM{params: map[string]string{path: testTenantKeyARN}}}
		got, err := r.resolveTenantKeyARN(context.Background(), p, cluster)
		if err != nil || got != testTenantKeyARN {
			t.Errorf("got %q / %v", got, err)
		}
	})

	t.Run("absent parameter is empty, not an error", func(t *testing.T) {
		r := &PlatformReconciler{SSM: &stubSSM{params: map[string]string{}}}
		got, err := r.resolveTenantKeyARN(context.Background(), p, cluster)
		if err != nil || got != "" {
			t.Errorf("a not-yet-applied substrate must not fail reconcile: %q / %v", got, err)
		}
	})

	t.Run("a real SSM failure propagates", func(t *testing.T) {
		r := &PlatformReconciler{SSM: &stubSSM{err: errors.New("throttled")}}
		if _, err := r.resolveTenantKeyARN(context.Background(), p, cluster); err == nil {
			t.Error("an SSM error is not a missing parameter and must not be swallowed")
		}
	})

	t.Run("no client or no cluster name resolves to nothing", func(t *testing.T) {
		if got, err := (&PlatformReconciler{}).resolveTenantKeyARN(context.Background(), p, cluster); err != nil || got != "" {
			t.Errorf("without an SSM client there is nothing to resolve: %q / %v", got, err)
		}
		r := &PlatformReconciler{SSM: &stubSSM{params: map[string]string{path: testTenantKeyARN}}}
		if got, err := r.resolveTenantKeyARN(context.Background(), p, ""); err != nil || got != "" {
			t.Errorf("without a cluster name the path cannot be composed: %q / %v", got, err)
		}
	})
}

func TestEnsureTenantKeyPolicy(t *testing.T) {
	const cluster = "development-platform"
	p := platformWithDatastores("myplat")
	path := tenantKeyParamPath(cluster, "myplat")
	cfg := IAMConfig{Environment: "development", Region: "us-west-2", ClusterName: cluster}

	t.Run("writes the scoped grant", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
		r := &PlatformReconciler{IAM: f, SSM: &stubSSM{params: map[string]string{path: testTenantKeyARN}}}

		if err := r.ensureTenantKeyPolicy(context.Background(), "test-role", p, cfg); err != nil {
			t.Fatalf("ensureTenantKeyPolicy: %v", err)
		}
		if len(f.putInlineCalls) != 1 {
			t.Fatalf("PutRolePolicy calls: got %d want 1", len(f.putInlineCalls))
		}
		if got := *f.putInlineCalls[0].PolicyName; got != tenantKeyPolicyName {
			t.Errorf("policy name: got %q want %q", got, tenantKeyPolicyName)
		}
		if doc := *f.putInlineCalls[0].PolicyDocument; !strings.Contains(doc, testTenantKeyARN) {
			t.Errorf("policy is not scoped to the published key: %s", doc)
		}
	})

	// A tenant whose key has not been published holds no KMS grant rather than a
	// broad one — and the policy is removed, so a cleared substrate leaves no
	// stale grant behind.
	t.Run("removes the policy when no key is published", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
		r := &PlatformReconciler{IAM: f, SSM: &stubSSM{params: map[string]string{}}}

		if err := r.ensureTenantKeyPolicy(context.Background(), "test-role", p, cfg); err != nil {
			t.Fatalf("ensureTenantKeyPolicy: %v", err)
		}
		if len(f.putInlineCalls) != 0 {
			t.Errorf("no key means no grant, got %d writes", len(f.putInlineCalls))
		}
	})

	t.Run("an SSM failure surfaces", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
		r := &PlatformReconciler{IAM: f, SSM: &stubSSM{err: errors.New("throttled")}}

		if err := r.ensureTenantKeyPolicy(context.Background(), "test-role", p, cfg); err == nil {
			t.Error("an SSM failure must surface, not degrade the tenant silently")
		}
		if len(f.putInlineCalls) != 0 {
			t.Errorf("no policy should be written when the lookup failed, got %d", len(f.putInlineCalls))
		}
	})

	t.Run("no IAM client is a no-op", func(t *testing.T) {
		r := &PlatformReconciler{}
		if err := r.ensureTenantKeyPolicy(context.Background(), "test-role", p, cfg); err != nil {
			t.Errorf("k8s-only test paths must not fail: %v", err)
		}
	})
}

// A Platform with no datastores still gets a key grant. The key backs
// application-side envelope encryption, which is independent of the datastore
// vocabulary — this is the case that would break if the grant ever moved into
// the datastore-access policy, which is absent entirely for such a tenant.
func TestTenantKeyPolicy_IndependentOfDatastores(t *testing.T) {
	noStores := platformWithDatastores("myplat")
	if len(noStores.Spec.Datastores) != 0 {
		t.Fatalf("fixture should declare no datastores")
	}

	f := newFakeIAM()
	f.seedRole("test-role", "arn:aws:iam::123456789012:role/test-role")
	cfg := IAMConfig{Environment: "development", Region: "us-west-2", ClusterName: "development-platform"}
	path := tenantKeyParamPath(cfg.ClusterName, "myplat")
	r := &PlatformReconciler{IAM: f, SSM: &stubSSM{params: map[string]string{path: testTenantKeyARN}}}

	if err := r.ensureTenantKeyPolicy(context.Background(), "test-role", noStores, cfg); err != nil {
		t.Fatalf("ensureTenantKeyPolicy: %v", err)
	}
	if len(f.putInlineCalls) != 1 {
		t.Fatalf("a datastore-less tenant must still get its key grant, got %d writes", len(f.putInlineCalls))
	}

	// And the datastore policy really is empty for this tenant, which is what
	// makes the separation load-bearing rather than stylistic.
	if stmts := datastorePolicyStatements(noStores, "development", testScope(), nil); len(stmts) != 0 {
		t.Errorf("fixture assumption broken: datastore policy should be empty, got %+v", stmts)
	}
}

// The two paths through ensureIamRole must both surface a key-policy failure.
// A tenant whose key grant silently failed to write looks healthy and then gets
// AccessDenied on its first envelope operation, which is exactly the failure
// mode this policy exists to remove.
func TestEnsureIamRole_TenantKeyPolicyError_CreatePath(t *testing.T) {
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f, SSM: &stubSSM{err: errors.New("throttled")}}
	p := newPlatform("app", "tenant")

	if _, err := r.ensureIamRole(context.Background(), p, datastoreErrCfg()); err == nil {
		t.Fatal("expected ensureIamRole to propagate the tenant-key-policy error on the create path")
	}
}

func TestEnsureIamRole_TenantKeyPolicyError_ExistingRolePath(t *testing.T) {
	f := newFakeIAM()
	cfg := datastoreErrCfg()
	r := &PlatformReconciler{IAM: f, SSM: &stubSSM{err: errors.New("throttled")}}
	p := newPlatform("app", "tenant")

	roleName := tenantRoleName(cfg.ClusterName, p)
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)

	if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
		t.Fatal("expected ensureIamRole to propagate the tenant-key-policy error on the existing-role path")
	}
}
