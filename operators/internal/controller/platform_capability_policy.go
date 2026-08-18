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
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

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

// capabilityGrantSids names the statement Sids capabilityPolicyStatements emits
// for each capability.
//
// It exists because a capability that grants nothing is indistinguishable, in
// the finished policy document, from a capability that was never declared: both
// contribute zero statements. Only the declaration and the document read
// together can tell them apart, and this is the mapping that joins them.
//
// Every Capability in the API belongs here. One missing would report as
// permanently ungranted, so the vocabulary test asserts this table and the API's
// capability constants are the same set — the table cannot silently fall behind
// a newly added capability.
var capabilityGrantSids = map[platformv1alpha1.Capability][]string{
	platformv1alpha1.CapabilitySES:                  {"sesSend", "sesQuota"},
	platformv1alpha1.CapabilityEventBridgeScheduler: {"schedulerManage", "schedulerPassInvokeRole"},
}

// ungrantedCapabilities returns the capabilities a Platform declares that
// produced no statement in the policy built for it, in declaration order.
//
// A capability resolves to nothing when its precondition is absent rather than
// when something failed — SES with no domain bound to the tenant is the shape
// this was written for. That is deliberate fail-closed behaviour (an absent
// binding must narrow the policy, never widen it), so it is not an error and
// must not fail the reconcile. But it is also not nothing: the operator asked
// for a capability and did not get it, and without this the Platform reports
// Ready while the grant is silently absent.
func ungrantedCapabilities(p *platformv1alpha1.Platform, stmts []policyStatement) []platformv1alpha1.Capability {
	granted := make(map[string]struct{}, len(stmts))
	for _, s := range stmts {
		granted[s.Sid] = struct{}{}
	}

	var ungranted []platformv1alpha1.Capability
	for _, c := range p.Spec.Identity.Capabilities {
		sids, known := capabilityGrantSids[c]
		if !known {
			// An undeclared-in-table capability is not reported: the CRD enum is
			// the gate on what may be set, and guessing here would turn a schema
			// problem into a per-tenant status message.
			continue
		}
		if !anySidPresent(sids, granted) {
			ungranted = append(ungranted, c)
		}
	}
	return ungranted
}

// anySidPresent reports whether any of sids appears in the built document.
// A capability counts as granted when it contributed any of its statements;
// partial emission is still a grant, and the policy builder never emits a
// strict subset by choice.
func anySidPresent(sids []string, granted map[string]struct{}) bool {
	for _, sid := range sids {
		if _, ok := granted[sid]; ok {
			return true
		}
	}
	return false
}

// schedulerInvokeRoleName is the EventBridge Scheduler invoke role the operator
// mints for a Platform declaring eventBridgeScheduler. The -scheduler-invoke
// suffix is what the agent-iam SchedulerInvokeRolePass boundary keys its
// PassRole allow-list on (role/*-scheduler-invoke), so the suffix is
// load-bearing. Cluster-scoped (not env-scoped) so two co-located clusters that
// share an account and environment can host a Platform of the same name without
// one cluster's delete tearing down the other's invoke role — the same
// isolation tenantRoleName gives the tenant role. Root path (deliberate: the
// PassRole grant composes role/<name> with no path segment). Capped at 64 chars
// with the same FNV-1a hash-truncation as tenantRoleName.
func schedulerInvokeRoleName(clusterName string, p *platformv1alpha1.Platform) string {
	const suffix = "-scheduler-invoke"
	const maxLen = 64
	full := clusterName + "-" + p.Name + suffix
	if len(full) <= maxLen {
		return full
	}
	prefix := clusterName + "-"
	// budget = maxLen - len(prefix) - len(suffix) - 1(hyphen) - 8(hash)
	budget := maxLen - len(prefix) - len(suffix) - 1 - 8
	h := fnv1a64(p.Name)
	return fmt.Sprintf("%s%s-%08x%s", prefix, p.Name[:budget], h&0xffffffff, suffix)
}

// schedulerInvokeRoleARN composes the invoke role's ARN from the same scope the
// tenant role resolves to, so the tenant-role PassRole grant and the app's
// SCHEDULER_ROLE_ARN name the same role.
func schedulerInvokeRoleARN(clusterName string, p *platformv1alpha1.Platform, scope arnScope) string {
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", scope.partition(), scope.account(), schedulerInvokeRoleName(clusterName, p))
}

// schedulerScheduleARN is the ARN pattern for the tenant's own schedules: every
// schedule in the tenant's own group. Both the tenant-role grant and the invoke
// role's trust condition key on it, so a tenant can only manage and fire its own
// schedules.
//
// The group, not a name prefix, is what makes that true. A schedule ARN is
// `schedule/<group>/<name>`, so naming the group is an exact match on that path
// segment. The prefix form this replaced — `schedule/default/<env>-<platform>-*`
// inside the shared default group — spanned whenever one Platform name was a
// hyphen-prefix of another: `foo` reached every schedule belonging to `foo-bar`,
// and Platform names are DNS-1123 labels with nothing forbidding that pair.
//
// ensureScheduleGroup creates the group, because a grant naming a group that
// does not exist is not a narrower grant, it is a dead path.
func schedulerScheduleARN(env string, p *platformv1alpha1.Platform, scope arnScope) string {
	return fmt.Sprintf("arn:%s:scheduler:%s:%s:schedule/%s/*", scope.partition(), scope.region(), scope.account(), scheduleGroupName(env, p))
}

// tenantQueueResources returns the exact SQS ARNs of the tenant's declared queue
// datastores, matching the tenant-substrate module's <env>-<platform>-<datastore>
// naming. The scheduler-invoke role's SendMessage is scoped to these, so Scheduler
// can only deliver into the tenant's own queues — which is a property of the
// enumeration, not of a prefix. See queueARNs for why a prefix does not deliver it.
func tenantQueueResources(p *platformv1alpha1.Platform, env string, scope arnScope) []string {
	var res []string
	for _, d := range p.Spec.Datastores {
		if d.Kind == platformv1alpha1.DatastoreQueue {
			base := fmt.Sprintf("%s-%s-%s", env, p.Name, d.Name)
			res = append(res, queueARNs(d, base, scope.partition(), scope.region(), scope.account())...)
		}
	}
	return res
}

// sesDomainParamPath is where the substrate publishes the verified sending
// domain bound to a tenant. landing-zone owns the SES identity — verifying a
// domain is account-level mail infra, and an identity is single-owner — so the
// binding is written there and only read here.
func sesDomainParamPath(clusterName, platform string) string {
	return fmt.Sprintf("/eks-agent-platform/%s/%s/ses/domain", clusterName, platform)
}

// resolveSESDomain returns the sending domain bound to this Platform, or the
// empty string when nothing is bound.
//
// A missing parameter is not an error, matching how the tenant-substrate secret
// lookups behave: the substrate applies independently of the operator, so a
// Platform can exist before its binding does. The consequence of an absent
// binding is a narrower policy, never a broader one — no domain means no SES
// statement, so the failure mode is a tenant that cannot send rather than one
// that can send as somebody else.
func (r *PlatformReconciler) resolveSESDomain(ctx context.Context, p *platformv1alpha1.Platform, clusterName string) (string, error) {
	if !hasCapability(p, platformv1alpha1.CapabilitySES) {
		return "", nil
	}
	if r.SSM == nil || clusterName == "" {
		return "", nil
	}
	return r.readSSMParameter(ctx, sesDomainParamPath(clusterName, p.Name))
}

// capabilityPolicyStatements builds the tenant-role grant statements for a
// Platform's declared capabilities. eventBridgeScheduler grants schedule
// management on the tenant's own schedules plus iam:PassRole on the minted
// invoke role, capped to the Scheduler service. clusterName scopes the invoke
// role; env scopes the schedule ARN prefix (schedules are env-keyed resources).
//
// sesDomain is the verified sending domain bound to this tenant, read from SSM
// by the caller. It is a parameter rather than something derived here because a
// verified identity is account-level mail infra owned by landing-zone, and the
// tenant must not be able to influence which one it reaches. An empty value
// means the binding does not exist, and no SES statement is emitted at all —
// declaring the capability grants nothing until the substrate binds a domain to
// it.
func capabilityPolicyStatements(p *platformv1alpha1.Platform, env, clusterName, sesDomain string, scope arnScope) []policyStatement {
	stmts := make([]policyStatement, 0, 4)

	if hasCapability(p, platformv1alpha1.CapabilitySES) && sesDomain != "" {
		stmts = append(stmts,
			policyStatement{
				Sid: "sesSend", Effect: "Allow",
				Action:   capabilitySESSendActions,
				Resource: []string{"*"},
				// Anchored at the domain: the wildcard covers the local part only.
				// A trailing wildcard would make the domain a prefix match, so a
				// tenant bound to "example.com" would also reach "example.com.evil"
				// — and one bound to a name that merely prefixes another tenant's
				// verified domain would inherit sending rights over it, silently.
				Condition: map[string]map[string]string{
					"StringLike": {"ses:FromAddress": fmt.Sprintf("*@%s", sesDomain)},
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
				Resource: []string{schedulerInvokeRoleARN(clusterName, p, scope)},
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
// The returned capabilities are those declared but granted nothing — see
// ungrantedCapabilities. They are a status signal, not an error: the reconcile
// succeeded and the policy on the role is correct for what the substrate
// currently supports.
func (r *PlatformReconciler) ensureCapabilityPolicy(ctx context.Context, roleName, roleARN string, p *platformv1alpha1.Platform, cfg IAMConfig) ([]platformv1alpha1.Capability, error) {
	if r.IAM == nil {
		return nil, nil
	}
	scope := arnScopeFromRole(roleARN, cfg.Region)

	// EventBridge Scheduler needs a service role Scheduler assumes to reach the
	// tenant's queues; mint or remove it to match the declaration.
	if hasCapability(p, platformv1alpha1.CapabilityEventBridgeScheduler) {
		if err := r.ensureSchedulerInvokeRole(ctx, p, cfg, scope); err != nil {
			return nil, err
		}
	} else if err := r.deleteSchedulerInvokeRole(ctx, p, cfg); err != nil {
		return nil, err
	}

	// The sending domain is resolved here rather than inside the policy builder
	// because it is a lookup against the substrate, not a property of the CR.
	sesDomain, err := r.resolveSESDomain(ctx, p, cfg.ClusterName)
	if err != nil {
		return nil, err
	}

	stmts := capabilityPolicyStatements(p, cfg.Environment, cfg.ClusterName, sesDomain, scope)
	desired, err := capabilityPolicyDoc(stmts)
	if err != nil {
		return nil, err //coverage:ignore only reachable if json.Marshal fails, which it cannot for this document
	}
	if err := r.reconcileInlinePolicy(ctx, roleName, capabilityPolicyName, desired); err != nil {
		return nil, err
	}
	return ungrantedCapabilities(p, stmts), nil
}

// ensureSchedulerInvokeRole mints (idempotently) the
// <cluster>-<platform>-scheduler-invoke role — trusted by the Scheduler service,
// carrying the tenant permissions boundary like every other operator-minted role
// — and reconciles its SendMessage policy against the tenant's queue datastores.
func (r *PlatformReconciler) ensureSchedulerInvokeRole(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig, scope arnScope) error {
	name := schedulerInvokeRoleName(cfg.ClusterName, p)

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
			Tags:                     schedulerInvokeRoleTags(p, cfg),
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

// schedulerInvokeRoleTags mirrors tenantRoleTags but marks Component as
// scheduler-invoke so a tag-based compromise sweep can tell the root-path
// invoke role apart from the path-prefixed tenant and session roles without
// knowing every name formula.
func schedulerInvokeRoleTags(p *platformv1alpha1.Platform, cfg IAMConfig) []iamtypes.Tag {
	tags := tenantRoleTags(p, cfg)
	for i := range tags {
		if aws.ToString(tags[i].Key) == "Component" {
			tags[i].Value = aws.String("scheduler-invoke")
		}
	}
	return tags
}

// deleteSchedulerInvokeRole removes the invoke role. Used both when the
// eventBridgeScheduler capability is dropped and on Platform finalization.
// detachAndDeleteRole tolerates a role that was never created, so it is a safe
// no-op for Platforms without the capability.
func (r *PlatformReconciler) deleteSchedulerInvokeRole(ctx context.Context, p *platformv1alpha1.Platform, cfg IAMConfig) error {
	if r.IAM == nil {
		return nil
	}
	return r.detachAndDeleteRole(ctx, schedulerInvokeRoleName(cfg.ClusterName, p))
}
