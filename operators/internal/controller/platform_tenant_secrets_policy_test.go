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

// TestTenantSecretsPolicy_ScopesToOwnPrefix proves the grant is scoped to the
// tenant's own <platform>/<env>/* secret prefix — a tenant can never read
// another's secrets.
func TestTenantSecretsPolicy_ScopesToOwnPrefix(t *testing.T) {
	doc, err := tenantSecretsPolicyDoc(platformWithCapabilities("myplat"), "development", testScope())
	if err != nil {
		t.Fatalf("tenantSecretsPolicyDoc: %v", err)
	}
	if !strings.Contains(doc, "arn:aws:secretsmanager:us-west-2:123456789012:secret:myplat/development/*") {
		t.Errorf("tenant-secrets grant not scoped to the tenant's own prefix: %s", doc)
	}
	if !strings.Contains(doc, "secretsmanager:GetSecretValue") {
		t.Errorf("tenant-secrets grant missing GetSecretValue: %s", doc)
	}
	if strings.Contains(doc, "secret:*") || strings.Contains(doc, `"*"`) {
		t.Errorf("tenant-secrets grant must not be a bare wildcard: %s", doc)
	}
}

// TestEnsureTenantSecretsPolicy_WritesAndConverges proves the reconcile writes
// the tenant-secrets policy once and no-ops on a converged re-run.
func TestEnsureTenantSecretsPolicy_WritesAndConverges(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}
	p := platformWithCapabilities("myplat")

	if err := r.ensureTenantSecretsPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("ensureTenantSecretsPolicy: %v", err)
	}
	puts := putsFor(f, tenantSecretsPolicyName)
	if len(puts) != 1 {
		t.Fatalf("tenant-secrets PutRolePolicy: got %d want 1", len(puts))
	}
	if !strings.Contains(*puts[0].PolicyDocument, "myplat/development/*") {
		t.Errorf("tenant-secrets policy not scoped to the tenant prefix: %s", *puts[0].PolicyDocument)
	}

	if err := r.ensureTenantSecretsPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := len(putsFor(f, tenantSecretsPolicyName)); got != 1 {
		t.Errorf("converged re-run must not re-write tenant-secrets: got %d", got)
	}
}

// TestEnsureTenantSecretsPolicy_NilIAM proves the reconcile no-ops without an
// IAM client.
func TestEnsureTenantSecretsPolicy_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if err := r.ensureTenantSecretsPolicy(context.Background(), "role", capRoleARN,
		platformWithCapabilities("myplat"), IAMConfig{}); err != nil {
		t.Fatalf("nil IAM must no-op: %v", err)
	}
}

// TestEnsureTenantSecretsPolicy_PutErrorPropagates proves a PutRolePolicy
// failure surfaces.
func TestEnsureTenantSecretsPolicy_PutErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.putInlineReturnsErr = map[string]error{tenantSecretsPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureTenantSecretsPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat"), capCfg()); err == nil {
		t.Fatalf("expected the PutRolePolicy error to propagate")
	}
}

// TestEnsureIamRole_TenantSecretsError_CreatePath proves ensureIamRole
// propagates a tenant-secrets failure on the create path.
func TestEnsureIamRole_TenantSecretsError_CreatePath(t *testing.T) {
	f := newFakeIAM()
	f.putInlineReturnsErr = map[string]error{tenantSecretsPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if _, err := r.ensureIamRole(context.Background(), newPlatform("app", "tenant"), datastoreErrCfg()); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the tenant-secrets error on the create path")
	}
}

// TestEnsureIamRole_TenantSecretsError_ExistingRolePath proves the same on the
// existing-role path.
func TestEnsureIamRole_TenantSecretsError_ExistingRolePath(t *testing.T) {
	f := newFakeIAM()
	cfg := datastoreErrCfg()
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	roleName := tenantRoleName(cfg.ClusterName, p)
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.putInlineReturnsErr = map[string]error{tenantSecretsPolicyName: errors.New("boom")}

	if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the tenant-secrets error on the existing-role path")
	}
}
