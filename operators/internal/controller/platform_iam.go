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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
)

// Suspension marker tag keys written by the kill-switch Step Functions
// state machine (see terraform/components/kill-switch). When the operator
// sees `suspendedTag=true` on a tenant role it stops reattaching the
// baseline policy + propagates Suspended phase + reason to the Platform
// CR. The kill-switch is the authoritative writer; the operator only
// observes.
const (
	suspendedTag       = "agents.stxkxs.io/suspended"
	suspendedReasonTag = "agents.stxkxs.io/suspended-reason"
)

// suspensionFromTags reads the suspension marker tags from a role's
// tag list. Returns (true, reason) when the marker is set; (false, "")
// otherwise. Reason is best-effort — the SFN sets it; if the role was
// suspended manually (operator removed the policy + tagged), the reason
// may be empty.
func suspensionFromTags(tags []iamtypes.Tag) (bool, string) {
	suspended := false
	reason := ""
	for _, t := range tags {
		switch aws.ToString(t.Key) {
		case suspendedTag:
			if aws.ToString(t.Value) == "true" {
				suspended = true
			}
		case suspendedReasonTag:
			reason = aws.ToString(t.Value)
		}
	}
	return suspended, reason
}

// platformSuspension is the return shape of ensureIamRole, carrying both
// the ARN and the kill-switch state. The PlatformReconciler propagates
// Suspended/Reason into status.suspendedAt + status.suspendedReason.
type platformSuspension struct {
	RoleARN   string
	Suspended bool
	Reason    string
}

// tenantRoleName returns the IAM role name minted for a Platform. Matches
// the contract documented in ADR 0003 and consumed by the kill-switch
// Step Functions state machine:
//
//	<env>-<platform.name>-tenant
//
// Capped at 64 chars (IAM role-name limit); long platform names get
// hash-truncated using the same scheme as PlatformNamespace.
func tenantRoleName(env string, p *agentsv1alpha1.Platform) string {
	const suffix = "-tenant"
	const maxLen = 64
	full := env + "-" + p.Name + suffix
	if len(full) <= maxLen {
		return full
	}
	prefix := env + "-"
	// budget = maxLen - len(prefix) - len(suffix) - 1(hyphen) - 8(hash)
	budget := maxLen - len(prefix) - len(suffix) - 1 - 8
	h := fnv1a64(p.Name)
	return fmt.Sprintf("%s%s-%08x%s", prefix, p.Name[:budget], h&0xffffffff, suffix)
}

// assumeRolePolicyForOIDC returns a trust policy JSON for an IRSA role.
// The federated principal is the EKS cluster's OIDC provider; the sub
// claim is constrained to the tenant ServiceAccount (one SA per Platform).
func assumeRolePolicyForOIDC(oidcProviderARN, oidcIssuerHost, namespace, serviceAccount string) (string, error) {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"Federated": oidcProviderARN},
			"Action":    "sts:AssumeRoleWithWebIdentity",
			"Condition": map[string]any{
				"StringEquals": map[string]any{
					oidcIssuerHost + ":sub": "system:serviceaccount:" + namespace + ":" + serviceAccount,
					oidcIssuerHost + ":aud": "sts.amazonaws.com",
				},
			},
		}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal trust policy: %w", err)
	}
	return string(b), nil
}

// IAMConfig is the subset of operatorconfig.Config needed for IAM
// reconciliation. Decoupled from operatorconfig so the reconciler doesn't
// import that package directly (keeps the package graph simple).
type IAMConfig struct {
	TenantIAMPath           string
	TenantBaselinePolicyARN string
	OIDCProviderARN         string
	OIDCIssuerHost          string // e.g. oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE
	Environment             string
}

// ensureIamRole creates (or no-ops if already present) the tenant IRSA
// role for a Platform, attaches the baseline policy, and returns the
// role ARN. The role's trust policy permits assumption only from the
// tenant ServiceAccount in the tenant workload namespace.
//
// Idempotent: re-runs on the same Platform observe the role's existence
// via GetRole and skip CreateRole. Reads the kill-switch suspension tag
// (agents.stxkxs.io/suspended); when present, returns
// platformSuspension{Suspended: true} and SKIPS attachBaselineIfMissing
// so the operator doesn't fight the kill-switch by reattaching the
// baseline policy on every reconcile.
func (r *PlatformReconciler) ensureIamRole(ctx context.Context, p *agentsv1alpha1.Platform, cfg IAMConfig) (platformSuspension, error) {
	if r.IAM == nil {
		// IAM client not wired (e.g., envtest path with no AWS creds).
		// Skip silently — AWS-side callers explicitly check IAM != nil.
		return platformSuspension{}, nil
	}
	if cfg.OIDCProviderARN == "" || cfg.OIDCIssuerHost == "" {
		return platformSuspension{}, fmt.Errorf("ensureIamRole: OIDCProviderARN and OIDCIssuerHost must be set in IAMConfig")
	}

	name := tenantRoleName(cfg.Environment, p)
	path := cfg.TenantIAMPath
	if path == "" {
		path = "/eks-agent-platform/tenants/"
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	// SA name convention: 'tenant-runtime' inside the tenant workload ns.
	// The AgentFleet reconciler creates that ServiceAccount when fleet
	// pods land; this function just establishes the trust contract so
	// pods using the SA can immediately AssumeRoleWithWebIdentity.
	const tenantSA = "tenant-runtime"
	trust, err := assumeRolePolicyForOIDC(cfg.OIDCProviderARN, cfg.OIDCIssuerHost, PlatformNamespace(p), tenantSA)
	if err != nil {
		return platformSuspension{}, err
	}

	// Idempotency: GetRole first; if NotFound, CreateRole.
	getOut, getErr := r.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if getErr == nil && getOut != nil && getOut.Role != nil {
		arn := aws.ToString(getOut.Role.Arn)
		suspended, reason := suspensionFromTags(getOut.Role.Tags)
		if suspended {
			// Kill-switch fired (or ops manually tagged the role).
			// Don't reattach the baseline — that would let the tenant
			// keep invoking Bedrock until the next SFN execution.
			return platformSuspension{RoleARN: arn, Suspended: true, Reason: reason}, nil
		}
		if err := r.reconcileManagedPolicies(ctx, name, cfg.TenantBaselinePolicyARN, p.Spec.Identity.ExtraPolicyArns); err != nil {
			return platformSuspension{RoleARN: arn}, err
		}
		return platformSuspension{RoleARN: arn}, nil
	}
	if !isIAMNotFound(getErr) {
		return platformSuspension{}, fmt.Errorf("iam GetRole: %w", getErr)
	}

	createOut, err := r.IAM.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		Path:                     aws.String(path),
		AssumeRolePolicyDocument: aws.String(trust),
		Description:              aws.String(fmt.Sprintf("Tenant IRSA role for Platform %s (tenant %s)", p.Name, p.Spec.Tenant)),
		Tags: []iamtypes.Tag{
			{Key: aws.String("PlatformId"), Value: aws.String(p.Name)},
			{Key: aws.String("Tenant"), Value: aws.String(p.Spec.Tenant)},
			{Key: aws.String("Persona"), Value: aws.String(p.Spec.Persona)},
			{Key: aws.String("Environment"), Value: aws.String(cfg.Environment)},
			{Key: aws.String("ManagedBy"), Value: aws.String("eks-agent-platform")},
		},
	})
	if err != nil {
		return platformSuspension{}, fmt.Errorf("iam CreateRole %s: %w", name, err)
	}
	arn := aws.ToString(createOut.Role.Arn)

	// Fresh role can't be suspended yet — go straight to attach.
	if err := r.reconcileManagedPolicies(ctx, name, cfg.TenantBaselinePolicyARN, p.Spec.Identity.ExtraPolicyArns); err != nil {
		return platformSuspension{RoleARN: arn}, err
	}
	return platformSuspension{RoleARN: arn}, nil
}

// reconcileManagedPolicies makes the set of attached managed policies on
// the tenant role match {baselineARN} ∪ extraPolicyArns. Idempotent: lists
// what's attached, attaches anything in the desired set that's missing.
// Does NOT detach attachments that aren't in the desired set — IAM has no
// per-attachment tag to mark "operator-owned", so blind detachment would
// fight any external policy the operator team attaches out-of-band.
// Spec-driven cleanup of removed ExtraPolicyArns is tracked separately.
//
// Paginates the list so roles approaching IAM's attach-policy soft-limit
// (≈20 by default, raisable to 50) still scan their full attachment set.
func (r *PlatformReconciler) reconcileManagedPolicies(ctx context.Context, roleName, baselineARN string, extraPolicyArns []string) error {
	desired := make(map[string]struct{}, 1+len(extraPolicyArns))
	if baselineARN != "" {
		desired[baselineARN] = struct{}{}
	}
	for _, arn := range extraPolicyArns {
		if arn != "" {
			desired[arn] = struct{}{}
		}
	}
	if len(desired) == 0 {
		return nil // dev/test mode with neither baseline nor extras configured
	}

	attached := make(map[string]struct{}, len(desired))
	var marker *string
	for {
		listOut, err := r.IAM.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
			RoleName: aws.String(roleName),
			Marker:   marker,
		})
		if err != nil {
			return fmt.Errorf("iam ListAttachedRolePolicies: %w", err)
		}
		for _, p := range listOut.AttachedPolicies {
			attached[aws.ToString(p.PolicyArn)] = struct{}{}
		}
		if !listOut.IsTruncated || listOut.Marker == nil {
			break
		}
		marker = listOut.Marker
	}

	for arn := range desired {
		if _, ok := attached[arn]; ok {
			continue
		}
		if _, err := r.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(arn),
		}); err != nil {
			return fmt.Errorf("iam AttachRolePolicy %s: %w", arn, err)
		}
	}
	return nil
}

// deleteIamRole is the finalizer counterpart: detach all policies and
// delete the role. Tolerates NotFound so re-runs are safe.
func (r *PlatformReconciler) deleteIamRole(ctx context.Context, p *agentsv1alpha1.Platform, environment string) error {
	if r.IAM == nil {
		return nil
	}
	name := tenantRoleName(environment, p)
	var marker *string
	for {
		listOut, err := r.IAM.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
			RoleName: aws.String(name),
			Marker:   marker,
		})
		if isIAMNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("iam ListAttachedRolePolicies: %w", err)
		}
		for _, ap := range listOut.AttachedPolicies {
			if _, err := r.IAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
				RoleName:  aws.String(name),
				PolicyArn: ap.PolicyArn,
			}); err != nil && !isIAMNotFound(err) {
				return fmt.Errorf("iam DetachRolePolicy: %w", err)
			}
		}
		if !listOut.IsTruncated || listOut.Marker == nil {
			break
		}
		marker = listOut.Marker
	}
	if _, err := r.IAM.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); err != nil && !isIAMNotFound(err) {
		return fmt.Errorf("iam DeleteRole: %w", err)
	}
	return nil
}

// isIAMNotFound returns true for IAM's NoSuchEntityException.
func isIAMNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "NoSuchEntity"
	}
	return false
}
