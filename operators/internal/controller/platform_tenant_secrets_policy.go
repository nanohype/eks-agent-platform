/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// tenantSecretsPolicyName is the inline policy granting a tenant role read
// access to the specific application secrets its pods resolve directly through
// the pod role, declared in spec.identity.directSecretReads. Most secret
// material reaches a tenant through the chart's ExternalSecret — projected by the
// External Secrets controller's own identity — and needs no grant here; this
// policy covers only the secrets a pod reads itself via the AWS SDK
// (rotation-sensitive values cached by VersionId, or config bulk-loaded at
// startup). Each declared name scopes to the tenant's own <platform>/<env>/<name>
// secret, so a tenant can never read another tenant's secrets, and a tenant that
// declares none holds no Secrets Manager grant at all.
const tenantSecretsPolicyName = "tenant-secrets"

var tenantSecretsActions = []string{"secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"}

// tenantSecretsPolicyDoc builds the inline policy from the declared direct-read
// secret names — one Resource ARN per entry, each under the tenant's own
// <platform>/<env>/ prefix. Returns the empty string when none are declared, so
// the caller (reconcileInlinePolicy) removes the inline policy. The trailing -*
// on each ARN covers the 6-char random suffix Secrets Manager appends to every
// secret name.
func tenantSecretsPolicyDoc(p *platformv1alpha1.Platform, env string, scope arnScope) (string, error) {
	names := p.Spec.Identity.DirectSecretReads
	if len(names) == 0 {
		return "", nil
	}
	resources := make([]string, 0, len(names))
	for _, name := range names {
		resources = append(resources, fmt.Sprintf("arn:%s:secretsmanager:%s:%s:secret:%s/%s/%s-*", scope.partition(), scope.region(), scope.account(), p.Name, env, name))
	}
	stmt := policyStatement{
		Sid:      "tenantSecrets",
		Effect:   "Allow",
		Action:   tenantSecretsActions,
		Resource: resources,
	}
	b, err := json.Marshal(policyDocument{Version: "2012-10-17", Statement: []policyStatement{stmt}})
	if err != nil {
		return "", fmt.Errorf("marshal tenant-secrets policy: %w", err) //coverage:ignore json.Marshal of a policyDocument of string fields cannot fail
	}
	return string(b), nil
}

// ensureTenantSecretsPolicy reconciles the tenant-secrets inline policy on a
// tenant role from spec.identity.directSecretReads. Removed when none are
// declared, so an ExternalSecret-only tenant holds no secretsmanager grant.
// Idempotent (read/diff/write via reconcileInlinePolicy). Callers MUST NOT invoke
// this on a suspended role — ensureIamRole's suspension short-circuit returns
// first, keeping the operator observe-only under the kill-switch.
func (r *PlatformReconciler) ensureTenantSecretsPolicy(ctx context.Context, roleName, roleARN string, p *platformv1alpha1.Platform, cfg IAMConfig) error {
	if r.IAM == nil {
		return nil
	}
	desired, err := tenantSecretsPolicyDoc(p, cfg.Environment, arnScopeFromRole(roleARN, cfg.Region))
	if err != nil {
		return err //coverage:ignore only reachable if json.Marshal fails, which it cannot for this document
	}
	return r.reconcileInlinePolicy(ctx, roleName, tenantSecretsPolicyName, desired)
}
