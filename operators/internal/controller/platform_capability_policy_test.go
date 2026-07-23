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

// platformWithCapabilities builds a Platform declaring the given capabilities
// (and optionally datastores, appended by the caller) for policy-generation
// unit tests.
func platformWithCapabilities(name string, caps ...platformv1alpha1.Capability) *platformv1alpha1.Platform { //nolint:unparam // policy-generation unit tests use a fixed platform token
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: platformv1alpha1.PlatformSpec{
			Identity: platformv1alpha1.IdentitySpec{Capabilities: caps},
		},
	}
}

const (
	capRoleARN     = "arn:aws:iam::123456789012:role/test-role"
	invokeRoleName = "development-myplat-scheduler-invoke"
)

func capCfg() IAMConfig {
	return IAMConfig{Environment: "development", Region: "us-west-2"}
}

// TestCapabilityPolicy_SES proves the ses capability grants scoped SendEmail
// (FromAddress-conditioned to the tenant's domain) plus the unconditioned
// account-global GetSendQuota, and nothing else.
func TestCapabilityPolicy_SES(t *testing.T) {
	stmts := capabilityPolicyStatements(platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), "development", testScope())

	send := findStmt(stmts, "sesSend")
	if send == nil || !hasResource(send, "*") {
		t.Fatalf("sesSend statement missing or misscoped: %+v", send)
	}
	if got := send.Condition["StringLike"]["ses:FromAddress"]; got != "*@myplat.*" {
		t.Errorf("ses:FromAddress condition: got %q want %q", got, "*@myplat.*")
	}
	quota := findStmt(stmts, "sesQuota")
	if quota == nil || len(quota.Condition) != 0 {
		t.Errorf("sesQuota must be unconditioned: %+v", quota)
	}
	// No scheduler statements when only ses is declared.
	if findStmt(stmts, "schedulerManage") != nil {
		t.Errorf("ses-only Platform must not grant scheduler")
	}
}

// TestCapabilityPolicy_Scheduler proves the eventBridgeScheduler capability
// grants schedule management on the tenant's own schedule prefix plus a
// service-capped PassRole on the minted invoke role.
func TestCapabilityPolicy_Scheduler(t *testing.T) {
	stmts := capabilityPolicyStatements(platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler), "development", testScope())

	manage := findStmt(stmts, "schedulerManage")
	if manage == nil || !hasResource(manage, "arn:aws:scheduler:us-west-2:123456789012:schedule/default/development-myplat-*") {
		t.Fatalf("schedulerManage missing or misscoped: %+v", manage)
	}
	pass := findStmt(stmts, "schedulerPassInvokeRole")
	if pass == nil || !hasResource(pass, "arn:aws:iam::123456789012:role/development-myplat-scheduler-invoke") {
		t.Fatalf("schedulerPassInvokeRole missing or misscoped: %+v", pass)
	}
	if got := pass.Condition["StringEquals"]["iam:PassedToService"]; got != "scheduler.amazonaws.com" {
		t.Errorf("PassRole must be capped to the Scheduler service, got %q", got)
	}
}

// TestCapabilityPolicy_BothAndNone proves both capabilities compose into one
// document, and no capability yields no document (so the reconciler removes the
// inline policy rather than writing an empty one).
func TestCapabilityPolicy_BothAndNone(t *testing.T) {
	both := capabilityPolicyStatements(
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES, platformv1alpha1.CapabilityEventBridgeScheduler),
		"development", testScope(),
	)
	for _, sid := range []string{"sesSend", "sesQuota", "schedulerManage", "schedulerPassInvokeRole"} {
		if findStmt(both, sid) == nil {
			t.Errorf("both-capabilities document missing %s", sid)
		}
	}
	doc, err := capabilityPolicyDoc(both)
	if err != nil {
		t.Fatalf("capabilityPolicyDoc: %v", err)
	}
	if doc == "" {
		t.Errorf("both capabilities must yield a non-empty document")
	}

	none := capabilityPolicyStatements(platformWithCapabilities("myplat"), "development", testScope())
	if len(none) != 0 {
		t.Errorf("no capability must yield no statements, got %d", len(none))
	}
	if d, _ := capabilityPolicyDoc(none); d != "" {
		t.Errorf("no statements must yield an empty document, got %q", d)
	}
}

// TestTenantQueueResources proves only queue datastores contribute an SQS ARN
// prefix (the scheduler-invoke role's send target), and other kinds are skipped.
func TestTenantQueueResources(t *testing.T) {
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{
		{Name: "nudges", Kind: platformv1alpha1.DatastoreQueue},
		{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
	}
	res := tenantQueueResources(p, "development", testScope())
	if len(res) != 1 || res[0] != "arn:aws:sqs:us-west-2:123456789012:development-myplat-nudges*" {
		t.Errorf("queue resources: got %v", res)
	}
}

// TestEnsureCapabilityPolicy_SESWritesAndConverges proves the ses reconcile
// writes the capability-access policy once and no-ops on a converged re-run.
func TestEnsureCapabilityPolicy_SESWritesAndConverges(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES)

	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	puts := putsFor(f, capabilityPolicyName)
	if len(puts) != 1 {
		t.Fatalf("capability-access PutRolePolicy: got %d want 1", len(puts))
	}
	if !strings.Contains(*puts[0].PolicyDocument, "ses:SendEmail") {
		t.Errorf("capability policy must grant ses:SendEmail: %s", *puts[0].PolicyDocument)
	}

	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := len(putsFor(f, capabilityPolicyName)); got != 1 {
		t.Errorf("converged re-run must not re-write capability-access: got %d", got)
	}
}

// TestEnsureCapabilityPolicy_RemovesWhenEmpty proves a Platform with no
// capability has the capability-access policy removed.
func TestEnsureCapabilityPolicy_RemovesWhenEmpty(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}

	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, platformWithCapabilities("myplat"), capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if len(putsFor(f, capabilityPolicyName)) != 0 {
		t.Errorf("no capability must write no capability-access policy")
	}
	if !deletedInline(f, capabilityPolicyName) {
		t.Errorf("no capability must delete the capability-access policy")
	}
}

// TestEnsureCapabilityPolicy_NilIAM proves the reconcile no-ops without an IAM
// client.
func TestEnsureCapabilityPolicy_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if err := r.ensureCapabilityPolicy(context.Background(), "role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), IAMConfig{}); err != nil {
		t.Fatalf("nil IAM must no-op: %v", err)
	}
}

// TestEnsureCapabilityPolicy_SchedulerMintsInvokeRole proves the scheduler
// capability mints the -scheduler-invoke role (boundaried) with a SendMessage
// policy scoped to the tenant's own queue datastores.
func TestEnsureCapabilityPolicy_SchedulerMintsInvokeRole(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}
	cfg := capCfg()
	cfg.TenantPermissionsBoundaryARN = "arn:aws:iam::123456789012:policy/tenant-boundary"
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "nudges", Kind: platformv1alpha1.DatastoreQueue}}

	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, cfg); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if len(f.createCalls) != 1 || *f.createCalls[0].RoleName != invokeRoleName {
		t.Fatalf("expected the scheduler-invoke role to be created: %+v", f.createCalls)
	}
	if f.createCalls[0].PermissionsBoundary == nil {
		t.Errorf("the invoke role must carry the tenant permissions boundary")
	}
	if !strings.Contains(*f.createCalls[0].AssumeRolePolicyDocument, "scheduler.amazonaws.com") {
		t.Errorf("the invoke role must be trusted by the Scheduler service")
	}
	send := putsFor(f, schedulerInvokeSendPolicyName)
	if len(send) != 1 || !strings.Contains(*send[0].PolicyDocument, "development-myplat-nudges") {
		t.Fatalf("the invoke role's send policy must target the tenant's queue: %+v", send)
	}
}

// TestEnsureCapabilityPolicy_SchedulerNoQueueRemovesSendPolicy proves a
// scheduler capability without any queue datastore mints the role but removes
// the (absent) send policy rather than writing an empty one.
func TestEnsureCapabilityPolicy_SchedulerNoQueueRemovesSendPolicy(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	r := &PlatformReconciler{IAM: f}
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)

	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, p, capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if len(putsFor(f, schedulerInvokeSendPolicyName)) != 0 {
		t.Errorf("no queue must write no send policy")
	}
	if !deletedInline(f, schedulerInvokeSendPolicyName) {
		t.Errorf("no queue must remove any stale send policy")
	}
}

// TestEnsureCapabilityPolicy_SchedulerRemovedDeletesInvokeRole proves dropping
// the scheduler capability tears down the invoke role.
func TestEnsureCapabilityPolicy_SchedulerRemovedDeletesInvokeRole(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	invoke := invokeRoleName
	f.seedRole(invoke, "arn:aws:iam::123456789012:role/"+invoke)
	r := &PlatformReconciler{IAM: f}

	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN, platformWithCapabilities("myplat"), capCfg()); err != nil {
		t.Fatalf("ensureCapabilityPolicy: %v", err)
	}
	if _, ok := f.roles[invoke]; ok {
		t.Errorf("dropping the scheduler capability must delete the invoke role")
	}
}

// TestEnsureCapabilityPolicy_GetErrorPropagates proves a non-NotFound
// GetRolePolicy failure on the capability-access reconcile surfaces.
func TestEnsureCapabilityPolicy_GetErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.getInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), capCfg()); err == nil {
		t.Fatalf("expected the GetRolePolicy error to propagate")
	}
}

// TestEnsureCapabilityPolicy_PutErrorPropagates proves a PutRolePolicy failure
// on the capability-access write surfaces.
func TestEnsureCapabilityPolicy_PutErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.putInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilitySES), capCfg()); err == nil {
		t.Fatalf("expected the PutRolePolicy error to propagate")
	}
}

// TestEnsureCapabilityPolicy_DeleteErrorPropagates proves a non-NotFound
// DeleteRolePolicy failure on the capability-access removal path surfaces.
func TestEnsureCapabilityPolicy_DeleteErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.inline["test-role"] = map[string]string{capabilityPolicyName: "{}"}
	f.deleteInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat"), capCfg()); err == nil {
		t.Fatalf("expected the DeleteRolePolicy error to propagate")
	}
}

// TestEnsureCapabilityPolicy_InvokeRoleGetErrorPropagates proves a non-NotFound
// GetRole failure while ensuring the invoke role surfaces.
func TestEnsureCapabilityPolicy_InvokeRoleGetErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.getReturnsErr = errors.New("boom")
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler), capCfg()); err == nil {
		t.Fatalf("expected the invoke-role GetRole error to propagate")
	}
}

// TestEnsureCapabilityPolicy_InvokeRoleCreateErrorPropagates proves a
// CreateRole failure while minting the invoke role surfaces.
func TestEnsureCapabilityPolicy_InvokeRoleCreateErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.createReturnsErr = errors.New("boom")
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler), capCfg()); err == nil {
		t.Fatalf("expected the invoke-role CreateRole error to propagate")
	}
}

// TestEnsureCapabilityPolicy_InvokeRoleDeleteErrorPropagates proves a failure
// tearing down the invoke role (capability dropped) surfaces.
func TestEnsureCapabilityPolicy_InvokeRoleDeleteErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.seedRole("test-role", capRoleARN)
	f.listReturnsErr = errors.New("boom")
	r := &PlatformReconciler{IAM: f}
	if err := r.ensureCapabilityPolicy(context.Background(), "test-role", capRoleARN,
		platformWithCapabilities("myplat"), capCfg()); err == nil {
		t.Fatalf("expected the invoke-role teardown error to propagate")
	}
}

// TestEnsureSchedulerInvokeRole_ExistingRoleSkipsCreate proves an existing
// invoke role is not re-created, only its send policy reconciled.
func TestEnsureSchedulerInvokeRole_ExistingRoleSkipsCreate(t *testing.T) {
	f := newFakeIAM()
	invoke := invokeRoleName
	f.seedRole(invoke, "arn:aws:iam::123456789012:role/"+invoke)
	r := &PlatformReconciler{IAM: f}
	p := platformWithCapabilities("myplat", platformv1alpha1.CapabilityEventBridgeScheduler)
	p.Spec.Datastores = []platformv1alpha1.DatastoreSpec{{Name: "nudges", Kind: platformv1alpha1.DatastoreQueue}}

	if err := r.ensureSchedulerInvokeRole(context.Background(), p, capCfg(), testScope()); err != nil {
		t.Fatalf("ensureSchedulerInvokeRole: %v", err)
	}
	if len(f.createCalls) != 0 {
		t.Errorf("an existing invoke role must not be re-created: %+v", f.createCalls)
	}
	if len(putsFor(f, schedulerInvokeSendPolicyName)) != 1 {
		t.Errorf("the send policy must still be reconciled on an existing role")
	}
}

// TestDeleteSchedulerInvokeRole_NilIAM proves the invoke-role teardown no-ops
// without an IAM client.
func TestDeleteSchedulerInvokeRole_NilIAM(t *testing.T) {
	r := &PlatformReconciler{}
	if err := r.deleteSchedulerInvokeRole(context.Background(), platformWithCapabilities("myplat"), capCfg()); err != nil {
		t.Fatalf("nil IAM must no-op: %v", err)
	}
}

// TestEnsureIamRole_CapabilityPolicyError_CreatePath proves ensureIamRole
// propagates a capability-policy failure on the create path.
func TestEnsureIamRole_CapabilityPolicyError_CreatePath(t *testing.T) {
	f := newFakeIAM()
	f.putInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	p.Spec.Identity.Capabilities = []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES}

	if _, err := r.ensureIamRole(context.Background(), p, datastoreErrCfg()); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the capability-policy error on the create path")
	}
}

// TestEnsureIamRole_CapabilityPolicyError_ExistingRolePath proves the same on
// the existing-role path.
func TestEnsureIamRole_CapabilityPolicyError_ExistingRolePath(t *testing.T) {
	f := newFakeIAM()
	cfg := datastoreErrCfg()
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("app", "tenant")
	p.Spec.Identity.Capabilities = []platformv1alpha1.Capability{platformv1alpha1.CapabilitySES}

	roleName := tenantRoleName(cfg.ClusterName, p)
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.putInlineReturnsErr = map[string]error{capabilityPolicyName: errors.New("boom")}

	if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
		t.Fatalf("expected ensureIamRole to propagate the capability-policy error on the existing-role path")
	}
}

// TestDeleteIamRole_SchedulerInvokeCleanupErrorPropagates proves the finalizer
// surfaces a failure tearing down the scheduler-invoke role rather than
// silently deleting the tenant role and leaving the invoke role orphaned.
func TestDeleteIamRole_SchedulerInvokeCleanupErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.listReturnsErr = errors.New("boom") // fails the invoke-role detachAndDeleteRole
	r := &PlatformReconciler{IAM: f}      // EKS nil -> pod-identity delete no-ops first
	p := newPlatform("app", "tenant")

	if err := r.deleteIamRole(context.Background(), p, capCfg()); err == nil {
		t.Fatalf("expected the scheduler-invoke cleanup error to propagate")
	}
}

// putsFor returns the PutRolePolicy calls for a given inline-policy name.
func putsFor(f *fakeIAM, policyName string) []iamPut {
	var out []iamPut
	for i := range f.putInlineCalls {
		if *f.putInlineCalls[i].PolicyName == policyName {
			out = append(out, iamPut{f.putInlineCalls[i].PolicyName, f.putInlineCalls[i].PolicyDocument})
		}
	}
	return out
}

type iamPut struct {
	PolicyName     *string
	PolicyDocument *string
}

// deletedInline reports whether a DeleteRolePolicy was issued for a policy name.
func deletedInline(f *fakeIAM, policyName string) bool {
	for i := range f.deleteInlineCalls {
		if *f.deleteInlineCalls[i].PolicyName == policyName {
			return true
		}
	}
	return false
}
