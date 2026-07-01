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
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/smithy-go"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// Suspension marker tag keys written by the kill-switch Step Functions
// state machine (see terraform/components/kill-switch). When the operator
// sees `suspendedTag=true` on a tenant role it stops reattaching the
// baseline policy + propagates Suspended phase + reason to the Platform
// CR. The kill-switch is the authoritative writer; the operator only
// observes.
const (
	suspendedTag       = "platform.nanohype.dev/suspended"
	suspendedReasonTag = "platform.nanohype.dev/suspended-reason"
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
func tenantRoleName(env string, p *platformv1alpha1.Platform) string {
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

// assumeRolePolicyForPodIdentity returns the trust policy JSON for a role
// assumed through EKS Pod Identity. The principal is the EKS service; the Pod
// Identity agent vends this role's credentials to pods whose ServiceAccount is
// bound to it by a PodIdentityAssociation (see ensurePodIdentityAssociation).
// The (namespace, service-account) constraint lives in the association, not the
// trust policy — so the trust policy itself is fixed.
func assumeRolePolicyForPodIdentity() (string, error) {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"Service": "pods.eks.amazonaws.com"},
			"Action":    []string{"sts:AssumeRole", "sts:TagSession"},
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
	// TenantPermissionsBoundaryARN, when set, is attached as the permissions
	// boundary on every tenant role the operator creates — capping a tenant
	// role's effective privileges regardless of which managed policies a
	// Platform CR requests. The operator's own IAM policy requires this
	// boundary on CreateRole/Attach, so it must be set on real clusters.
	TenantPermissionsBoundaryARN string
	// ClusterName is the EKS cluster the tenant Pod Identity association
	// targets. The operator binds the tenant ServiceAccount to its role with a
	// PodIdentityAssociation on this cluster, so it must be set on real clusters
	// (the EKS client errors without it).
	ClusterName string
	Environment string
	// Region is the operator's home region, used to mint the
	// inference-profile ARN patterns in the bedrock-model-scoping policy
	// (profiles are account+region resources). Empty wildcards the region.
	Region string

	// Org-dimension tag values for tenant roles (resource-tagging
	// standard, required tier). Sourced from the operator's deploy config
	// (AGENTS_COST_CENTER / _BUSINESS_UNIT / _DATA_CLASSIFICATION / _COMPLIANCE).
	// tenantRoleTags falls back to the landing-zone env.hcl defaults when these
	// are unset, so a tenant role always carries the keys cloudgov gates on.
	CostCenter         string
	BusinessUnit       string
	DataClassification string
	Compliance         string
}

// orDefault returns def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// tenantRoleTags builds the IAM tag set for a tenant role.
//
// It preserves the keys the rest of the system depends on and must not rename:
// PlatformId (the BudgetPolicy reconciler groups Cost Explorer by it), Tenant,
// and Persona. On top of those it carries the required-tier resource-tagging
// keys cloudgov gates on — Project, Repository, Component, Team, CostCenter,
// BusinessUnit, DataClassification, Compliance — plus Environment and ManagedBy.
// ManagedBy is "eks-agent-platform" (the operator owns these roles' lifecycle,
// unlike the opentofu-managed roles in landing-zone).
func tenantRoleTags(p *platformv1alpha1.Platform, cfg IAMConfig) []iamtypes.Tag {
	tag := func(k, v string) iamtypes.Tag {
		return iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)}
	}
	return []iamtypes.Tag{
		// Load-bearing keys — PlatformId drives BudgetPolicy cost attribution.
		tag("PlatformId", p.Name),
		tag("Tenant", p.Spec.Tenant),
		tag("Persona", p.Spec.Persona),
		// Required-tier resource-tagging keys.
		tag("Environment", cfg.Environment),
		tag("ManagedBy", "eks-agent-platform"),
		tag("Project", "eks-agent-platform"),
		tag("Repository", "nanohype/eks-agent-platform"),
		tag("Component", "tenant-iam"),
		tag("Team", p.Spec.Tenant),
		tag("CostCenter", orDefault(cfg.CostCenter, "platform-engineering")),
		tag("BusinessUnit", orDefault(cfg.BusinessUnit, "engineering")),
		tag("DataClassification", orDefault(cfg.DataClassification, "internal")),
		tag("Compliance", orDefault(cfg.Compliance, "soc2")),
	}
}

// ensureIamRole creates (or no-ops if already present) the tenant role
// for a Platform, attaches the baseline policy, reconciles the
// bedrock-model-scoping inline policy from spec.identity, binds the tenant
// ServiceAccount to it via a Pod Identity association, and returns the
// role ARN.
//
// Idempotent: re-runs on the same Platform observe the role's existence
// via GetRole and skip CreateRole. Reads the kill-switch suspension tag
// (platform.nanohype.dev/suspended); when present, returns
// platformSuspension{Suspended: true} and SKIPS both the managed-policy
// reconcile and the model-scoping policy write so the operator doesn't
// fight the kill-switch by reattaching grants on every reconcile.
func (r *PlatformReconciler) ensureIamRole(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig) (platformSuspension, error) {
	if r.IAM == nil {
		// IAM client not wired (e.g., envtest path with no AWS creds).
		// Skip silently — AWS-side callers explicitly check IAM != nil.
		return platformSuspension{}, nil
	}
	name := tenantRoleName(cfg.Environment, p)
	path := cfg.TenantIAMPath
	if path == "" {
		path = "/eks-agent-platform/tenants/"
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	// The tenant ServiceAccount (tenantSAName, created by the AgentFleet /
	// AgentSandbox reconcilers) is bound to this role by a Pod Identity
	// association below; the trust policy itself is the fixed EKS-service trust.
	trust, err := assumeRolePolicyForPodIdentity()
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
			// Don't reattach the baseline and don't touch the
			// bedrock-model-scoping policy — the operator is observe-only
			// on a suspended role; any policy write here would fight the
			// kill-switch until the next SFN execution.
			return platformSuspension{RoleARN: arn, Suspended: true, Reason: reason}, nil
		}
		if err := r.reconcileManagedPolicies(ctx, name, cfg.TenantBaselinePolicyARN, p.Spec.Identity.ExtraPolicyArns); err != nil {
			return platformSuspension{RoleARN: arn}, err
		}
		if err := r.ensureModelScopingPolicy(ctx, name, arn, p.Spec.Identity, cfg); err != nil {
			return platformSuspension{RoleARN: arn}, err
		}
		if err := r.ensurePodIdentityAssociation(ctx, cfg, PlatformNamespace(p), tenantSAName, arn); err != nil {
			return platformSuspension{RoleARN: arn}, err
		}
		return platformSuspension{RoleARN: arn}, nil
	}
	if !isIAMNotFound(getErr) {
		return platformSuspension{}, fmt.Errorf("iam GetRole: %w", getErr)
	}

	createInput := &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		Path:                     aws.String(path),
		AssumeRolePolicyDocument: aws.String(trust),
		Description:              aws.String(fmt.Sprintf("Tenant role for Platform %s (tenant %s)", p.Name, p.Spec.Tenant)),
		Tags:                     tenantRoleTags(p, cfg),
	}
	if cfg.TenantPermissionsBoundaryARN != "" {
		createInput.PermissionsBoundary = aws.String(cfg.TenantPermissionsBoundaryARN)
	}
	createOut, err := r.IAM.CreateRole(ctx, createInput)
	if err != nil {
		return platformSuspension{}, fmt.Errorf("iam CreateRole %s: %w", name, err)
	}
	arn := aws.ToString(createOut.Role.Arn)

	// Fresh role can't be suspended yet — go straight to attach.
	if err := r.reconcileManagedPolicies(ctx, name, cfg.TenantBaselinePolicyARN, p.Spec.Identity.ExtraPolicyArns); err != nil {
		return platformSuspension{RoleARN: arn}, err
	}
	if err := r.ensureModelScopingPolicy(ctx, name, arn, p.Spec.Identity, cfg); err != nil {
		return platformSuspension{RoleARN: arn}, err
	}
	if err := r.ensurePodIdentityAssociation(ctx, cfg, PlatformNamespace(p), tenantSAName, arn); err != nil {
		return platformSuspension{RoleARN: arn}, err
	}
	return platformSuspension{RoleARN: arn}, nil
}

// ensurePodIdentityAssociation binds the tenant ServiceAccount to its IAM role
// through EKS Pod Identity, so pods using the SA receive the role's credentials
// with no role-arn annotation. Idempotent: it lists associations for the
// (namespace, serviceAccount) on the cluster first and creates one only when
// none exists. A nil EKS client (envtest / dev without AWS) is a silent no-op,
// mirroring the IAM-nil short-circuit.
func (r *PlatformReconciler) ensurePodIdentityAssociation(ctx context.Context, cfg IAMConfig, namespace, serviceAccount, roleARN string) error {
	if r.EKS == nil {
		return nil
	}
	if cfg.ClusterName == "" {
		return fmt.Errorf("ensurePodIdentityAssociation: ClusterName must be set in IAMConfig")
	}
	listOut, err := r.EKS.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{
		ClusterName:    aws.String(cfg.ClusterName),
		Namespace:      aws.String(namespace),
		ServiceAccount: aws.String(serviceAccount),
	})
	if err != nil {
		return fmt.Errorf("eks ListPodIdentityAssociations: %w", err)
	}
	if len(listOut.Associations) > 0 {
		return nil
	}
	if _, err := r.EKS.CreatePodIdentityAssociation(ctx, &eks.CreatePodIdentityAssociationInput{
		ClusterName:    aws.String(cfg.ClusterName),
		Namespace:      aws.String(namespace),
		ServiceAccount: aws.String(serviceAccount),
		RoleArn:        aws.String(roleARN),
	}); err != nil {
		return fmt.Errorf("eks CreatePodIdentityAssociation: %w", err)
	}
	return nil
}

// deletePodIdentityAssociation removes the Pod Identity association for the
// tenant ServiceAccount so the binding doesn't outlive the role. Tolerates a
// nil EKS client and a missing association so finalizer re-runs are safe.
func (r *PlatformReconciler) deletePodIdentityAssociation(ctx context.Context, cfg IAMConfig, namespace, serviceAccount string) error {
	if r.EKS == nil || cfg.ClusterName == "" {
		return nil
	}
	listOut, err := r.EKS.ListPodIdentityAssociations(ctx, &eks.ListPodIdentityAssociationsInput{
		ClusterName:    aws.String(cfg.ClusterName),
		Namespace:      aws.String(namespace),
		ServiceAccount: aws.String(serviceAccount),
	})
	if err != nil {
		return fmt.Errorf("eks ListPodIdentityAssociations: %w", err)
	}
	for _, a := range listOut.Associations {
		if _, err := r.EKS.DeletePodIdentityAssociation(ctx, &eks.DeletePodIdentityAssociationInput{
			ClusterName:   aws.String(cfg.ClusterName),
			AssociationId: a.AssociationId,
		}); err != nil {
			return fmt.Errorf("eks DeletePodIdentityAssociation: %w", err)
		}
	}
	return nil
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

// deleteIamRole is the finalizer counterpart: remove the Pod Identity
// association, detach all policies, and delete the tenant role. Tolerates a
// missing association and NotFound so re-runs are safe.
func (r *PlatformReconciler) deleteIamRole(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig) error {
	if err := r.deletePodIdentityAssociation(ctx, cfg, PlatformNamespace(p), tenantSAName); err != nil {
		return err
	}
	return r.detachAndDeleteRole(ctx, tenantRoleName(cfg.Environment, p))
}

// detachAndDeleteRole detaches every managed policy from a role, deletes its
// inline policies (IAM refuses DeleteRole while the bedrock-model-scoping
// policy remains), and deletes it. Shared by the tenant-role and session-role
// finalizers. Tolerates NotFound at every step so re-runs (and roles that
// were never created) are safe no-ops.
func (r *PlatformReconciler) detachAndDeleteRole(ctx context.Context, name string) error {
	if r.IAM == nil {
		return nil
	}
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
	if err := r.deleteInlinePolicies(ctx, name); err != nil {
		return err
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
