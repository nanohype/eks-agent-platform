/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The capability policy is the counterpart to datastore-access (see
// platform_datastore_policy.go) for grants the datastore vocabulary does not
// cover. spec.identity.capabilities names what the tenant needs (ses,
// eventBridgeScheduler); the operator generates the scoped IAM from that
// declaration by the same <env>-<platform> naming convention — so SES and
// EventBridge Scheduler ride the declaration too, not a hand-written managed
// policy referenced through extraPolicyArns.
const (
	// capabilityPolicyName is the inline policy carrying the tenant-role grants
	// for the declared capabilities.
	capabilityPolicyName = "capability-access"

	// schedulerInvokeSendPolicyName is the inline policy on the minted
	// scheduler-invoke role granting SendMessage to the tenant's own queues.
	schedulerInvokeSendPolicyName = "sqs-send"
)

var (
	capabilitySESSendActions   = []string{"ses:SendEmail", "ses:SendRawEmail"}
	capabilitySchedulerActions = []string{
		"scheduler:CreateSchedule", "scheduler:GetSchedule",
		"scheduler:UpdateSchedule", "scheduler:DeleteSchedule",
	}
)

// hasCapability reports whether a Platform declares a capability.
func hasCapability(p *platformv1alpha1.Platform, c platformv1alpha1.Capability) bool {
	for _, x := range p.Spec.Identity.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// schedulerInvokeRoleName is the EventBridge Scheduler invoke role the operator
// mints for a Platform declaring eventBridgeScheduler. The -scheduler-invoke
// suffix is what the agent-iam SchedulerInvokeRolePass boundary keys its
// PassRole allow-list on, so the name is load-bearing. Root path, so the ARN is
// role/<env>-<platform>-scheduler-invoke and the tenant grant that passes it
// scopes to exactly that.
func schedulerInvokeRoleName(env string, p *platformv1alpha1.Platform) string {
	return fmt.Sprintf("%s-%s-scheduler-invoke", env, p.Name)
}

// schedulerInvokeRoleARN composes the invoke role's ARN from the same scope the
// tenant role resolves to, so the tenant-role PassRole grant and the app's
// SCHEDULER_ROLE_ARN name the same role.
func schedulerInvokeRoleARN(env string, p *platformv1alpha1.Platform, scope arnScope) string {
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", scope.partition(), scope.account(), schedulerInvokeRoleName(env, p))
}

// schedulerScheduleARN is the ARN pattern for the tenant's own schedules: the
// default schedule group, scoped to the <env>-<platform>- name prefix. Both the
// tenant-role grant and the invoke role's trust condition key on it, so a
// tenant can only manage and fire its own schedules.
func schedulerScheduleARN(env string, p *platformv1alpha1.Platform, scope arnScope) string {
	return fmt.Sprintf("arn:%s:scheduler:%s:%s:schedule/default/%s-%s-*", scope.partition(), scope.region(), scope.account(), env, p.Name)
}

// tenantQueueResources returns the SQS ARN prefixes for the tenant's declared
// queue datastores (matching the tenant-substrate module's <env>-<platform>-
// <datastore> naming; the trailing * covers .fifo and the DLQ). The
// scheduler-invoke role's SendMessage is scoped to exactly these — Scheduler can
// only deliver into the tenant's own queues.
func tenantQueueResources(p *platformv1alpha1.Platform, env string, scope arnScope) []string {
	var res []string
	for _, d := range p.Spec.Datastores {
		if d.Kind == platformv1alpha1.DatastoreQueue {
			base := fmt.Sprintf("%s-%s-%s", env, p.Name, d.Name)
			res = append(res, fmt.Sprintf("arn:%s:sqs:%s:%s:%s*", scope.partition(), scope.region(), scope.account(), base))
		}
	}
	return res
}

// capabilityPolicyStatements builds the tenant-role grant statements for a
// Platform's declared capabilities. ses is scoped by a ses:FromAddress condition
// to the tenant's sending domain (the verified identity itself is account-level
// mail infra, not provisioned here); eventBridgeScheduler grants schedule
// management on the tenant's own schedules plus iam:PassRole on the minted
// invoke role, capped to the Scheduler service.
func capabilityPolicyStatements(p *platformv1alpha1.Platform, env string, scope arnScope) []policyStatement {
	stmts := make([]policyStatement, 0, 4)

	if hasCapability(p, platformv1alpha1.CapabilitySES) {
		stmts = append(stmts,
			policyStatement{
				Sid: "sesSend", Effect: "Allow",
				Action:   capabilitySESSendActions,
				Resource: []string{"*"},
				Condition: map[string]map[string]string{
					"StringLike": {"ses:FromAddress": fmt.Sprintf("*@%s.*", p.Name)},
				},
			},
			policyStatement{
				Sid: "sesQuota", Effect: "Allow",
				// GetSendQuota is account-global (no resource/condition scope); it
				// can't ride the FromAddress-conditioned statement or the condition
				// would deny it.
				Action:   []string{"ses:GetSendQuota"},
				Resource: []string{"*"},
			},
		)
	}

	if hasCapability(p, platformv1alpha1.CapabilityEventBridgeScheduler) {
		stmts = append(stmts,
			policyStatement{
				Sid: "schedulerManage", Effect: "Allow",
				Action:   capabilitySchedulerActions,
				Resource: []string{schedulerScheduleARN(env, p, scope)},
			},
			policyStatement{
				Sid: "schedulerPassInvokeRole", Effect: "Allow",
				Action:   []string{"iam:PassRole"},
				Resource: []string{schedulerInvokeRoleARN(env, p, scope)},
				Condition: map[string]map[string]string{
					"StringEquals": {"iam:PassedToService": "scheduler.amazonaws.com"},
				},
			},
		)
	}

	return stmts
}

// capabilityPolicyDoc marshals the statements into an IAM policy document, or
// returns the empty string when there are none (the caller removes the inline
// policy in that case).
func capabilityPolicyDoc(stmts []policyStatement) (string, error) {
	if len(stmts) == 0 {
		return "", nil
	}
	b, err := json.Marshal(policyDocument{Version: "2012-10-17", Statement: stmts})
	if err != nil {
		return "", fmt.Errorf("marshal capability policy: %w", err) //coverage:ignore json.Marshal of a policyDocument of string fields cannot fail
	}
	return string(b), nil
}

// schedulerInvokeTrustPolicy returns the trust policy for the invoke role: the
// EventBridge Scheduler service, constrained by aws:SourceAccount and an
// aws:SourceArn ArnLike on the tenant's own schedule prefix, so only the
// tenant's schedules can assume it.
func schedulerInvokeTrustPolicy(env string, p *platformv1alpha1.Platform, scope arnScope) (string, error) {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"Service": "scheduler.amazonaws.com"},
			"Action":    "sts:AssumeRole",
			"Condition": map[string]any{
				"StringEquals": map[string]any{"aws:SourceAccount": scope.account()},
				"ArnLike":      map[string]any{"aws:SourceArn": schedulerScheduleARN(env, p, scope)},
			},
		}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal scheduler trust policy: %w", err) //coverage:ignore unreachable — static-typed document of strings
	}
	return string(b), nil
}

// reconcileInlinePolicy converges one inline policy on a role: it writes the
// desired document when it drifts and removes the policy when desired is empty.
// Shared by the tenant-role capability policy and the invoke role's send policy.
// Idempotent — reads the current document and writes only on drift; NotFound is
// tolerated at every step so re-runs and cleared declarations are safe.
func (r *PlatformReconciler) reconcileInlinePolicy(ctx context.Context, roleName, policyName, desired string) error {
	if desired == "" {
		if _, err := r.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName:   aws.String(roleName),
			PolicyName: aws.String(policyName),
		}); err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("iam DeleteRolePolicy %s/%s: %w", roleName, policyName, err)
		}
		return nil
	}

	getOut, getErr := r.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
		RoleName:   aws.String(roleName),
		PolicyName: aws.String(policyName),
	})
	if getErr == nil && getOut != nil && policyDocEqual(aws.ToString(getOut.PolicyDocument), desired) {
		return nil
	}
	if getErr != nil && !isIAMNotFound(getErr) {
		return fmt.Errorf("iam GetRolePolicy %s/%s: %w", roleName, policyName, getErr)
	}

	if _, err := r.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(desired),
	}); err != nil {
		return fmt.Errorf("iam PutRolePolicy %s/%s: %w", roleName, policyName, err)
	}
	return nil
}

// ensureCapabilityPolicy reconciles the capability-access inline policy on a
// tenant role from spec.identity.capabilities, and mints or removes the
// scheduler-invoke role to match. When no capability is declared the inline
// policy is removed so a cleared declaration leaves no stale grant. Callers MUST
// NOT invoke this on a suspended role — ensureIamRole's suspension
// short-circuit returns first, keeping the operator observe-only under the
// kill-switch.
func (r *PlatformReconciler) ensureCapabilityPolicy(ctx context.Context, roleName, roleARN string, p *platformv1alpha1.Platform, cfg IAMConfig) error {
	if r.IAM == nil {
		return nil
	}
	scope := arnScopeFromRole(roleARN, cfg.Region)

	// EventBridge Scheduler needs a service role Scheduler assumes to reach the
	// tenant's queues; mint or remove it to match the declaration.
	if hasCapability(p, platformv1alpha1.CapabilityEventBridgeScheduler) {
		if err := r.ensureSchedulerInvokeRole(ctx, p, cfg, scope); err != nil {
			return err
		}
	} else if err := r.deleteSchedulerInvokeRole(ctx, p, cfg); err != nil {
		return err
	}

	stmts := capabilityPolicyStatements(p, cfg.Environment, scope)
	desired, err := capabilityPolicyDoc(stmts)
	if err != nil {
		return err //coverage:ignore only reachable if json.Marshal fails, which it cannot for this document
	}
	return r.reconcileInlinePolicy(ctx, roleName, capabilityPolicyName, desired)
}

// ensureSchedulerInvokeRole mints (idempotently) the <env>-<platform>-scheduler-invoke
// role — trusted by the Scheduler service, carrying the tenant permissions
// boundary like every other operator-minted role — and reconciles its SendMessage
// policy against the tenant's queue datastores.
func (r *PlatformReconciler) ensureSchedulerInvokeRole(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig, scope arnScope) error {
	name := schedulerInvokeRoleName(cfg.Environment, p)

	_, getErr := r.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if getErr != nil {
		if !isIAMNotFound(getErr) {
			return fmt.Errorf("iam GetRole %s: %w", name, getErr)
		}
		trust, err := schedulerInvokeTrustPolicy(cfg.Environment, p, scope)
		if err != nil {
			return err //coverage:ignore unreachable — schedulerInvokeTrustPolicy marshals a static document
		}
		createInput := &iam.CreateRoleInput{
			RoleName:                 aws.String(name),
			AssumeRolePolicyDocument: aws.String(trust),
			Description:              aws.String(fmt.Sprintf("EventBridge Scheduler invoke role for Platform %s (tenant %s)", p.Name, p.Spec.Tenant)),
			Tags:                     tenantRoleTags(p, cfg),
		}
		if cfg.TenantPermissionsBoundaryARN != "" {
			createInput.PermissionsBoundary = aws.String(cfg.TenantPermissionsBoundaryARN)
		}
		if _, err := r.IAM.CreateRole(ctx, createInput); err != nil {
			return fmt.Errorf("iam CreateRole %s: %w", name, err)
		}
	}

	res := tenantQueueResources(p, cfg.Environment, scope)
	var desired string
	if len(res) > 0 {
		doc, err := capabilityPolicyDoc([]policyStatement{{
			Sid: "schedulerTargetSend", Effect: "Allow",
			Action: []string{"sqs:SendMessage"}, Resource: res,
		}})
		if err != nil {
			return err //coverage:ignore only reachable if json.Marshal fails, which it cannot for this document
		}
		desired = doc
	}
	return r.reconcileInlinePolicy(ctx, name, schedulerInvokeSendPolicyName, desired)
}

// deleteSchedulerInvokeRole removes the invoke role. Used both when the
// eventBridgeScheduler capability is dropped and on Platform finalization.
// detachAndDeleteRole tolerates a role that was never created, so it is a safe
// no-op for Platforms without the capability.
func (r *PlatformReconciler) deleteSchedulerInvokeRole(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig) error {
	if r.IAM == nil {
		return nil
	}
	return r.detachAndDeleteRole(ctx, schedulerInvokeRoleName(cfg.Environment, p))
}
