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
// access to its OWN application secrets under <platform>/<env>/* in Secrets
// Manager. Every tenant reads its own seeded config secrets (Slack tokens,
// provider keys, webhook HMAC secrets, ...) — either directly via the pod role or
// through the chart's ExternalSecret. Unlike datastore-access (which only grants
// the RDS-managed master secret, whose name is AWS-generated), this grant is
// universal and scoped to the tenant's own prefix, so a tenant can never read
// another tenant's secrets.
const tenantSecretsPolicyName = "tenant-secrets"

var tenantSecretsActions = []string{"secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"}

// tenantSecretsPolicyDoc builds the inline policy document granting the tenant
// its own <platform>/<env>/* secret prefix. Always non-empty — every tenant owns
// a secret namespace. The trailing * covers the 6-char random suffix Secrets
// Manager appends to each secret name.
func tenantSecretsPolicyDoc(p *platformv1alpha1.Platform, env string, scope arnScope) (string, error) {
	stmt := policyStatement{
		Sid:    "tenantSecrets",
		Effect: "Allow",
		Action: tenantSecretsActions,
		Resource: []string{
			fmt.Sprintf("arn:%s:secretsmanager:%s:%s:secret:%s/%s/*", scope.partition(), scope.region(), scope.account(), p.Name, env),
		},
	}
	b, err := json.Marshal(policyDocument{Version: "2012-10-17", Statement: []policyStatement{stmt}})
	if err != nil {
		return "", fmt.Errorf("marshal tenant-secrets policy: %w", err) //coverage:ignore json.Marshal of a policyDocument of string fields cannot fail
	}
	return string(b), nil
}

// ensureTenantSecretsPolicy reconciles the tenant-secrets inline policy on a
// tenant role. Always present — every tenant reads its own <platform>/<env>/*
// secrets. Idempotent (read/diff/write via reconcileInlinePolicy). Callers MUST
// NOT invoke this on a suspended role — ensureIamRole's suspension short-circuit
// returns first, keeping the operator observe-only under the kill-switch.
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
