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

// platformWithSecretReads builds a Platform declaring the given
// directSecretReads for tenant-secrets policy-generation unit tests.
func platformWithSecretReads(name string, reads ...string) *platformv1alpha1.Platform { //nolint:unparam // policy-generation unit tests use a fixed platform token
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: platformv1alpha1.PlatformSpec{
			Identity: platformv1alpha1.IdentitySpec{DirectSecretReads: reads},
		},
	}
}

// TestTenantSecretsPolicy_ScopesToDeclaredSecrets proves the grant lists exactly
// the declared secrets, each scoped to the tenant's own <platform>/<env>/<name>
// ARN — never a prefix wildcard, never a bare wildcard.
func TestTenantSecretsPolicy_ScopesToDeclaredSecrets(t *testing.T) {
	doc, err := tenantSecretsPolicyDoc(
		platformWithSecretReads("myplat", "grafana/oncall-webhook-hmac", "config"), "development", testScope())
	if err != nil {
		t.Fatalf("tenantSecretsPolicyDoc: %v", err)
	}
	for _, want := range []string{
		"arn:aws:secretsmanager:us-west-2:123456789012:secret:myplat/development/grafana/oncall-webhook-hmac-*",
		"arn:aws:secretsmanager:us-west-2:123456789012:secret:myplat/development/config-*",
		"secretsmanager:GetSecretValue",
		"secretsmanager:DescribeSecret",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("tenant-secrets grant missing %q: %s", want, doc)
		}
	}
	// The retired universal grant scoped the whole prefix; the declaration form
	// must not reintroduce it, nor a bare wildcard.
	if strings.Contains(doc, `development/*"`) {
		t.Errorf("tenant-secrets grant must not scope the whole prefix: %s", doc)
	}
	if strings.Contains(doc, `"*"`) {
		t.Errorf("tenant-secrets grant must not be a bare wildcard: %s", doc)
	}
}

// TestTenantSecretsPolicy_EmptyWhenNoneDeclared proves a tenant that declares no
// direct reads produces no policy document — the caller removes the inline
// policy, leaving no Secrets Manager grant.
func TestTenantSecretsPolicy_EmptyWhenNoneDeclared(t *testing.T) {
	doc, err := tenantSecretsPolicyDoc(platformWithSecretReads("myplat"), "development", testScope())
	if err != nil {
		t.Fatalf("tenantSecretsPolicyDoc: %v", err)
	}
	if doc != "" {
		t.Errorf("no directSecretReads must yield an empty document, got: %s", doc)
	}
}

// TestEnsureTenantSecretsPolicy_WritesDeclaredAndConverges proves the reconcile
// writes the declared secrets once and no-ops on a converged re-run.
func TestEnsureTenantSecretsPolicy_WritesDeclaredAndConverges(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}
	p := platformWithSecretReads("myplat", "config")

	if err := r.ensureTenantSecretsPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("ensureTenantSecretsPolicy: %v", err)
	}
	puts := putsFor(f, tenantSecretsPolicyName)
	if len(puts) != 1 {
		t.Fatalf("tenant-secrets PutRolePolicy: got %d want 1", len(puts))
	}
	if !strings.Contains(*puts[0].PolicyDocument, "myplat/development/config-*") {
		t.Errorf("tenant-secrets policy not scoped to the declared secret: %s", *puts[0].PolicyDocument)
	}

	if err := r.ensureTenantSecretsPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := len(putsFor(f, tenantSecretsPolicyName)); got != 1 {
		t.Errorf("converged re-run must not re-write tenant-secrets: got %d", got)
	}
}

// TestEnsureTenantSecretsPolicy_RemovedWhenNoneDeclared proves an
// ExternalSecret-only tenant (no directSecretReads) gets the inline policy
// removed rather than a universal grant.
func TestEnsureTenantSecretsPolicy_RemovedWhenNoneDeclared(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}

	if err := r.ensureTenantSecretsPolicy(context.Background(), "test-role", capRoleARN,
		platformWithSecretReads("myplat"), capCfg()); err != nil {
		t.Fatalf("ensureTenantSecretsPolicy: %v", err)
	}
	if got := len(putsFor(f, tenantSecretsPolicyName)); got != 0 {
		t.Errorf("no directSecretReads must not write a grant: got %d puts", got)
	}
	if !deletedInline(f, tenantSecretsPolicyName) {
		t.Errorf("no directSecretReads must remove any stale tenant-secrets policy")
	}
}

// TestEnsureTenantSecretsPolicy_NilIAM proves the reconcile no-ops without an
// IAM client.
func TestEnsureTenantSecretsPolicy_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if err := r.ensureTenantSecretsPolicy(context.Background(), "role", capRoleARN,
		platformWithSecretReads("myplat", "config"), IAMConfig{}); err != nil {
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
		platformWithSecretReads("myplat", "config"), capCfg()); err == nil {
		t.Fatalf("expected the PutRolePolicy error to propagate")
	}
}

// TestEnsureIamRole_TenantSecretsError_CreatePath proves ensureIamRole
// propagates a tenant-secrets failure on the create path.
func TestEnsureIamRole_TenantSecretsError_CreatePath(t *testing.T) {
	f := newFakeIAM()
	f.putInlineReturnsErr = map[string]error{tenantSecretsPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	p.Spec.Identity.DirectSecretReads = []string{"config"}
	if _, err := r.ensureIamRole(context.Background(), p, datastoreErrCfg()); err == nil {
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
	p.Spec.Identity.DirectSecretReads = []string{"config"}
	roleName := tenantRoleName(cfg.ClusterName, p)
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.putInlineReturnsErr = map[string]error{tenantSecretsPolicyName: errors.New("boom")}

	if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the tenant-secrets error on the existing-role path")
	}
}
