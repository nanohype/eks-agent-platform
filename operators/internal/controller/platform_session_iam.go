/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// defaultSessionRoleMaxDuration is the assumed-session lifetime when a
// Platform's spec.attribution.sessionRoleMaxDurationSeconds is unset. Matches
// the STS role-chaining ceiling: the caller is the pod's own IRSA-assumed
// tenant role, so AWS caps the chained session at 3600s regardless.
const defaultSessionRoleMaxDuration int32 = 3600

// sessionRoleName returns the attribution session role minted for a Platform:
//
//	<env>-<platform.name>-session
//
// Same 64-char cap + FNV-1a hash-truncation scheme as tenantRoleName, so the
// two role names never collide and both stay within IAM's role-name limit.
func sessionRoleName(clusterName string, p *platformv1alpha1.Platform) string {
	const suffix = "-session"
	const maxLen = 64
	full := clusterName + "-" + p.Name + suffix
	if len(full) <= maxLen {
		return full
	}
	prefix := clusterName + "-"
	budget := maxLen - len(prefix) - len(suffix) - 1 - 8
	h := fnv1a64(p.Name)
	return fmt.Sprintf("%s%s-%08x%s", prefix, p.Name[:budget], h&0xffffffff, suffix)
}

// sessionRoleTrustPolicy builds the trust policy for the attribution session
// role: only the tenant IRSA role may assume it, and only while setting an STS
// SourceIdentity drawn from the Platform's operator list. sts:SetSourceIdentity
// is granted alongside sts:AssumeRole so the caller can stamp the human, and
// the sts:SourceIdentity condition pins the allowed values so the caller can't
// assume the role under an arbitrary identity.
func sessionRoleTrustPolicy(tenantRoleARN string, operators []string) (string, error) {
	stmt := map[string]any{
		"Effect":    "Allow",
		"Principal": map[string]any{"AWS": tenantRoleARN},
		"Action":    []string{"sts:AssumeRole", "sts:SetSourceIdentity"},
	}
	if len(operators) > 0 {
		stmt["Condition"] = map[string]any{
			"StringEquals": map[string]any{"sts:SourceIdentity": operators},
		}
	}
	doc := map[string]any{
		"Version":   "2012-10-17",
		"Statement": []map[string]any{stmt},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal session trust policy: %w", err)
	}
	return string(b), nil
}

// sessionRoleTags mirrors tenantRoleTags but marks the role's Component as
// session-iam so cloudgov tagging + cost attribution tell the two roles apart.
func sessionRoleTags(p *platformv1alpha1.Platform, cfg IAMConfig) []iamtypes.Tag {
	tags := tenantRoleTags(p, cfg)
	for i := range tags {
		if aws.ToString(tags[i].Key) == "Component" {
			tags[i].Value = aws.String("session-iam")
		}
	}
	return tags
}

// sessionRoleMaxDuration reads the per-Platform cap, defaulting to 3600.
func sessionRoleMaxDuration(p *platformv1alpha1.Platform) int32 {
	if p.Spec.Attribution != nil && p.Spec.Attribution.SessionRoleMaxDurationSeconds != nil {
		return *p.Spec.Attribution.SessionRoleMaxDurationSeconds
	}
	return defaultSessionRoleMaxDuration
}

// ensureSessionRole provisions (or reconciles) the attribution session role for
// a Platform with spec.attribution set, and returns its ARN. The role is
// assumable only by the tenant IRSA role, only while carrying one of the
// Platform's operators as STS SourceIdentity, and is limited to the tenant
// baseline policy (Bedrock invoke) clamped by the same bedrock-model-scoping
// policy as the tenant role — never broad sts:AssumeRole.
//
// When suspended (kill-switch), the baseline is DETACHED rather than attached:
// otherwise a suspended tenant could keep invoking Bedrock through the session
// role even after its own tenant role's baseline was pulled.
//
// Idempotent: refreshes the trust policy on every reconcile (the operator list
// can change) and converges the baseline attachment to the suspended state.
func (r *PlatformReconciler) ensureSessionRole(
	ctx context.Context,
	p *platformv1alpha1.Platform,
	tenantRoleARN string,
	suspended bool,
	cfg IAMConfig,
) (string, error) {
	if r.IAM == nil || p.Spec.Attribution == nil {
		return "", nil
	}
	name := sessionRoleName(cfg.ClusterName, p)
	trust, err := sessionRoleTrustPolicy(tenantRoleARN, p.Spec.Attribution.Operators)
	if err != nil {
		return "", err
	}

	// Idempotency: GetRole first; if present, refresh trust + converge baseline.
	getOut, getErr := r.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if getErr == nil && getOut != nil && getOut.Role != nil {
		arn := aws.ToString(getOut.Role.Arn)
		if _, err := r.IAM.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
			RoleName:       aws.String(name),
			PolicyDocument: aws.String(trust),
		}); err != nil {
			return arn, fmt.Errorf("iam UpdateAssumeRolePolicy %s: %w", name, err)
		}
		if err := r.reconcileSessionBaseline(ctx, name, cfg.TenantBaselinePolicyARN, suspended); err != nil {
			return arn, err
		}
		if err := r.reconcileSessionModelScoping(ctx, name, arn, p, suspended, cfg); err != nil {
			return arn, err
		}
		return arn, nil
	}
	if !isIAMNotFound(getErr) {
		return "", fmt.Errorf("iam GetRole %s: %w", name, getErr)
	}

	path := cfg.TenantIAMPath
	if path == "" {
		path = "/eks-agent-platform/tenants/"
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	createInput := &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		Path:                     aws.String(path),
		AssumeRolePolicyDocument: aws.String(trust),
		Description:              aws.String(fmt.Sprintf("Attribution session role for Platform %s (tenant %s)", p.Name, p.Spec.Tenant)),
		MaxSessionDuration:       aws.Int32(sessionRoleMaxDuration(p)),
		Tags:                     sessionRoleTags(p, cfg),
	}
	if cfg.TenantPermissionsBoundaryARN != "" {
		createInput.PermissionsBoundary = aws.String(cfg.TenantPermissionsBoundaryARN)
	}
	createOut, err := r.IAM.CreateRole(ctx, createInput)
	if err != nil {
		return "", fmt.Errorf("iam CreateRole %s: %w", name, err)
	}
	arn := aws.ToString(createOut.Role.Arn)
	if err := r.reconcileSessionBaseline(ctx, name, cfg.TenantBaselinePolicyARN, suspended); err != nil {
		return arn, err
	}
	if err := r.reconcileSessionModelScoping(ctx, name, arn, p, suspended, cfg); err != nil {
		return arn, err
	}
	return arn, nil
}

// reconcileSessionBaseline converges the session role's baseline attachment:
// attached when running, detached when suspended (kill-switch parity with the
// tenant role). No-op when no baseline policy is configured (dev/test).
func (r *PlatformReconciler) reconcileSessionBaseline(ctx context.Context, roleName, baselineARN string, suspended bool) error {
	if baselineARN == "" {
		return nil
	}
	if suspended {
		if _, err := r.IAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(baselineARN),
		}); err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("iam DetachRolePolicy %s (suspend session role): %w", baselineARN, err)
		}
		return nil
	}
	return r.reconcileManagedPolicies(ctx, roleName, baselineARN, nil)
}

// reconcileSessionModelScoping applies the same bedrock-model-scoping inline
// policy the tenant role carries to the attribution session role. Without it,
// a session identity — which attaches the same broad baseline — would be a
// bypass around the Platform's allowedModelFamilies/allowedModels boundary.
// Skipped while suspended (observe-only, matching ensureIamRole).
func (r *PlatformReconciler) reconcileSessionModelScoping(ctx context.Context, roleName, roleARN string, p *platformv1alpha1.Platform, suspended bool, cfg IAMConfig) error {
	if suspended {
		return nil
	}
	return r.ensureModelScopingPolicy(ctx, roleName, roleARN, p.Spec.Identity, cfg)
}

// deleteSessionRole is the finalizer counterpart: detach policies + delete the
// session role. Tolerates NotFound so non-attribution Platforms (which never
// had a session role) and re-runs are safe.
func (r *PlatformReconciler) deleteSessionRole(ctx context.Context, p *platformv1alpha1.Platform, clusterName string) error {
	return r.detachAndDeleteRole(ctx, sessionRoleName(clusterName, p))
}
