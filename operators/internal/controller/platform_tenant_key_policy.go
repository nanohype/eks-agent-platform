/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// tenantKeyPolicyName is the inline policy granting a tenant role use of its own
// customer-managed key.
//
// Separate from datastore-access on purpose. The key is not a datastore: the
// tenant-substrate component mints one per tenant whether or not that tenant
// declares any store, because application-side envelope encryption is
// independent of the datastore vocabulary. Folding it into datastore-access
// would mean a tenant with no datastores — which produces no datastore policy at
// all — silently loses its key grant.
const tenantKeyPolicyName = "tenant-key-access"

// tenantKeyActions is what envelope encryption needs and nothing more. No
// Encrypt: the pattern is GenerateDataKey to obtain a wrapped one-shot key, then
// Decrypt to unwrap it on read. No key-management verbs — the tenant uses the
// key, the substrate owns it.
var tenantKeyActions = []string{
	"kms:GenerateDataKey",
	"kms:Decrypt",
	"kms:DescribeKey",
}

// tenantKeyPolicyStatements grants use of exactly one key ARN. An empty ARN
// yields no statements, so the caller removes the policy — the same fail-closed
// shape the relational secret grant uses. Granting on a pattern instead would
// reach every tenant's key, which is the isolation break this key exists to
// create in the first place.
func tenantKeyPolicyStatements(keyARN string) []policyStatement {
	if keyARN == "" {
		return nil
	}
	return []policyStatement{{
		Sid:      "tenantKeyEnvelope",
		Effect:   "Allow",
		Action:   tenantKeyActions,
		Resource: []string{keyARN},
	}}
}

// ssm-producer: landing-zone's tenant-substrate component, aws_ssm_parameter
// "tenant_kms_key_arn", one per tenant whether or not it declares a datastore.
// tenantKeyParamPath is where tenant-substrate publishes the tenant's own key
// ARN — the same /eks-agent-platform/<cluster>/tenant-substrate/<tenant>/ subtree
// the master-secret ARN uses.
func tenantKeyParamPath(clusterName, platform string) string {
	return fmt.Sprintf("/eks-agent-platform/%s/tenant-substrate/%s/kms_key_arn",
		clusterName, platform)
}

// resolveTenantKeyARN reads the published key ARN. A missing parameter is not an
// error: the substrate applies independently of the CR, so a Platform can
// reconcile before its key exists. The tenant simply holds no KMS grant until it
// does, which is correct — the alternative is granting on a pattern that spans
// tenants.
func (r *PlatformReconciler) resolveTenantKeyARN(
	ctx context.Context, p *platformv1alpha1.Platform, clusterName string,
) (string, error) {
	if r.SSM == nil || clusterName == "" {
		return "", nil
	}
	return r.readSSMParameter(ctx, tenantKeyParamPath(clusterName, p.Name))
}

// ensureTenantKeyPolicy reconciles the tenant-key-access inline policy. Callers
// MUST NOT invoke this on a suspended role — ensureIamRole's suspension
// short-circuit returns first, keeping the operator observe-only under the
// kill-switch.
func (r *PlatformReconciler) ensureTenantKeyPolicy(
	ctx context.Context, roleName string, p *platformv1alpha1.Platform, cfg IAMConfig,
) error {
	if r.IAM == nil {
		return nil
	}

	keyARN, err := r.resolveTenantKeyARN(ctx, p, cfg.ClusterName)
	if err != nil {
		return err
	}

	desired, err := datastorePolicyDoc(tenantKeyPolicyStatements(keyARN))
	if err != nil {
		return err //coverage:ignore only reachable if json.Marshal fails, which it cannot for this document
	}
	return r.reconcileInlinePolicy(ctx, roleName, tenantKeyPolicyName, desired)
}
