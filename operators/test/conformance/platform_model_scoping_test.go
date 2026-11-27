/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

// scopingEnv is the cluster name and environment string the model-scoping
// conformance tests run their reconciler under; role names follow the ADR 0003
// contract <cluster-name>-<platform>-tenant.
const scopingEnv = "conformance"

// scopingPolicyName mirrors the operator's inline policy name (the CRD/ADR
// contract, restated here so a rename upstream breaks these tests loudly).
const scopingPolicyName = "bedrock-model-scoping"

// phaseSuspended mirrors controller.phaseSuspended (unexported there), in the
// same way modelgateway_reconciler_test.go restates phaseReady.
const phaseSuspended = "Suspended"

// condModelAccessScoped is the Platform status condition the reconciler
// writes for the reconciled Bedrock model boundary.
const condModelAccessScoped = "ModelAccessScoped"

// tenantIAMPath is the IAM path the reconciler mints a Platform's tenant,
// session and eval roles under when IAMConfig names none. A test standing in
// for one of those roles made out of band seeds it there.
const tenantIAMPath = "/eks-agent-platform/tenants/"

// newScopingReconciler wires a PlatformReconciler with the in-memory IAM. EKS
// / KMS / S3 stay nil so the reconcile exercises exactly the k8s surface +
// the IAM role/policy path under test.
func newScopingReconciler(iamFake *awsfake.IAM) *controller.PlatformReconciler {
	return &controller.PlatformReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
		IAM:         iamFake,
		IAMCfg: controller.IAMConfig{
			TenantBaselinePolicyARN: "arn:aws:iam::123456789012:policy/eks-agent-platform/conformance-tenant-baseline",
			ClusterName:             scopingEnv,
			Environment:             scopingEnv,
			Region:                  "us-west-2",
		},
	}
}

// reconcileIAM drives Reconcile to convergence for a reconciler with a wired
// IAM client. Unlike reconcileOnce, convergence is the periodic poll requeue
// (the reconciler always re-queues to poll for out-of-band kill-switch tag
// changes when IAM != nil). The poll is jittered off a 60s base to desync many
// Platforms, so convergence is any requeue in the [60s, 72s] jitter window.
func reconcileIAM(ctx context.Context, t *testing.T, r *controller.PlatformReconciler, p *platformv1alpha1.Platform) {
	t.Helper()
	for i := 0; i < 5; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}})
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter >= 60*time.Second && res.RequeueAfter <= 72*time.Second {
			return
		}
	}
	t.Fatalf("reconcile did not converge in 5 attempts")
}

// shortScopingName derives a short (collision-hashed) Platform name from the
// test name. Short deliberately: tenantRoleFor below restates the ADR 0003
// role-name contract WITHOUT its 64-char hash-truncation branch, so the name
// must keep <cluster-name>-<name>-tenant under IAM's 64-char role-name limit.
func shortScopingName(t *testing.T) string {
	h := uint64(1469598103934665603)
	for i := 0; i < len(t.Name()); i++ {
		h ^= uint64(t.Name()[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("scope-%08x", h&0xffffffff)
}

// scopingPlatform builds a Platform CR for these tests and registers cleanup
// of the tenant namespace the reconciler provisions.
func scopingPlatform(ctx context.Context, t *testing.T, identity platformv1alpha1.IdentitySpec) *platformv1alpha1.Platform {
	t.Helper()
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: shortScopingName(t), Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "generic",
			Tenant:   "acme",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: identity,
		},
	}
	mustCreate(ctx, t, p)
	tenantNS := controller.PlatformNamespace(p)
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}) })
	return p
}

// tenantRoleFor mirrors the ADR 0003 role-name contract
// <cluster-name>-<platform>-tenant. scopingEnv doubles as the reconciler's
// cluster name (IAMCfg.ClusterName above), so the tenant role is prefixed with it.
func tenantRoleFor(p *platformv1alpha1.Platform) string {
	return scopingEnv + "-" + p.Name + "-tenant"
}

// scopingStatement unmarshals the single statement out of a scoping document.
func scopingStatement(t *testing.T, doc string) map[string]any {
	t.Helper()
	var parsed struct {
		Statement []map[string]any `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("unmarshal scoping doc %q: %v", doc, err)
	}
	if len(parsed.Statement) != 1 {
		t.Fatalf("scoping doc statements: got %d want 1 (%s)", len(parsed.Statement), doc)
	}
	return parsed.Statement[0]
}

func TestPlatformReconciler_ModelScopingPolicyCreatedFromSpec(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	iamFake := awsfake.NewIAM()
	r := newScopingReconciler(iamFake)
	p := scopingPlatform(ctx, t, platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}})

	reconcileIAM(ctx, t, r, p)

	doc, ok := iamFake.InlinePolicy(tenantRoleFor(p), scopingPolicyName)
	if !ok {
		t.Fatalf("expected %s inline policy on %s", scopingPolicyName, tenantRoleFor(p))
	}
	stmt := scopingStatement(t, doc)
	if stmt["Effect"] != "Deny" || stmt["Sid"] != "DenyUnscopedBedrockInvoke" {
		t.Errorf("statement: got %+v", stmt)
	}
	if !strings.Contains(doc, "arn:aws:bedrock:*::foundation-model/anthropic.*") {
		t.Errorf("doc missing the anthropic foundation-model pattern: %s", doc)
	}
	if !strings.Contains(doc, "arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.anthropic.*") {
		t.Errorf("doc missing the cross-region us. inference-profile pattern: %s", doc)
	}

	// Status surfaces the boundary.
	got := getPlatform(ctx, t, p.Namespace, p.Name)
	var cond *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == condModelAccessScoped {
			cond = &got.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Scoped" {
		t.Errorf("expected ModelAccessScoped=True/Scoped condition; got %+v", got.Status.Conditions)
	}
	if !strings.Contains(cond.Message, "anthropic") {
		t.Errorf("condition message should name the families: %q", cond.Message)
	}
}

func TestPlatformReconciler_ModelScopingPolicyFollowsSpecChange(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	iamFake := awsfake.NewIAM()
	r := newScopingReconciler(iamFake)
	p := scopingPlatform(ctx, t, platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}})

	reconcileIAM(ctx, t, r, p)

	// Converged: another pass must not rewrite the document.
	before := iamFake.PutCount()
	reconcileIAM(ctx, t, r, p)
	if iamFake.PutCount() != before {
		t.Errorf("converged reconcile rewrote the policy: %d extra PutRolePolicy calls", iamFake.PutCount()-before)
	}

	// Tighten from a family to one explicit model.
	got := getPlatform(ctx, t, p.Namespace, p.Name)
	got.Spec.Identity.AllowedModelFamilies = nil
	got.Spec.Identity.AllowedModels = []string{"anthropic.claude-sonnet-4-6"}
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("update spec: %v", err)
	}
	reconcileIAM(ctx, t, r, got)

	doc, ok := iamFake.InlinePolicy(tenantRoleFor(p), scopingPolicyName)
	if !ok {
		t.Fatalf("scoping policy disappeared on spec change")
	}
	if !strings.Contains(doc, "foundation-model/anthropic.claude-sonnet-4-6*") ||
		!strings.Contains(doc, "inference-profile/us.anthropic.claude-sonnet-4-6*") {
		t.Errorf("doc did not tighten to the explicit model: %s", doc)
	}
	if strings.Contains(doc, "foundation-model/anthropic.*") {
		t.Errorf("family-wide pattern must be gone after tightening: %s", doc)
	}
}

func TestPlatformReconciler_ModelScopingGrantRemovedWhenFieldsCleared(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	iamFake := awsfake.NewIAM()
	r := newScopingReconciler(iamFake)
	p := scopingPlatform(ctx, t, platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic", "amazon-nova"}})

	reconcileIAM(ctx, t, r, p)

	got := getPlatform(ctx, t, p.Namespace, p.Name)
	got.Spec.Identity.AllowedModelFamilies = nil
	got.Spec.Identity.AllowedModels = nil
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatalf("clear identity spec: %v", err)
	}
	reconcileIAM(ctx, t, r, got)

	// The model grant is removed; what remains is the deny-everything clamp
	// so the baseline's wildcard bedrock:InvokeModel stays unreachable
	// (deny-by-default — an unset spec must never widen back to the
	// baseline's Resource "*").
	doc, ok := iamFake.InlinePolicy(tenantRoleFor(p), scopingPolicyName)
	if !ok {
		t.Fatalf("clamp must remain when fields are cleared (deny-by-default)")
	}
	stmt := scopingStatement(t, doc)
	if stmt["Sid"] != "DenyAllBedrockInvoke" || stmt["Effect"] != "Deny" {
		t.Errorf("cleared spec should render the deny-all clamp: %+v", stmt)
	}
	if strings.Contains(doc, "NotResource") || strings.Contains(doc, "anthropic") || strings.Contains(doc, "nova") {
		t.Errorf("no model grant may survive a cleared spec: %s", doc)
	}

	// Condition flips to deny-by-default.
	got = getPlatform(ctx, t, p.Namespace, p.Name)
	var cond *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == condModelAccessScoped {
			cond = &got.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Reason != "DenyByDefault" {
		t.Errorf("expected ModelAccessScoped reason DenyByDefault; got %+v", cond)
	}
}

func TestPlatformReconciler_ModelScopingAbsentUnderKillSwitchSuspension(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	iamFake := awsfake.NewIAM()
	r := newScopingReconciler(iamFake)
	p := scopingPlatform(ctx, t, platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}})

	// Kill-switch fired before the operator ever reconciled this Platform:
	// the role exists and carries the suspension marker.
	iamFake.SeedRoleAtPath(tenantIAMPath, tenantRoleFor(p),
		iamtypes.Tag{Key: aws.String("platform.nanohype.dev/suspended"), Value: aws.String("true")},
		iamtypes.Tag{Key: aws.String("platform.nanohype.dev/suspended-reason"), Value: aws.String("budget-exceeded")},
	)

	reconcileIAM(ctx, t, r, p)

	if _, ok := iamFake.InlinePolicy(tenantRoleFor(p), scopingPolicyName); ok {
		t.Errorf("suspended role must not receive the model scoping policy")
	}
	if iamFake.PutCount() != 0 {
		t.Errorf("no PutRolePolicy may run against a suspended role; got %d", iamFake.PutCount())
	}

	got := getPlatform(ctx, t, p.Namespace, p.Name)
	if got.Status.Phase != phaseSuspended {
		t.Errorf("status.phase: got %q want Suspended", got.Status.Phase)
	}
	if got.Status.SuspendedReason != "budget-exceeded" {
		t.Errorf("status.suspendedReason: got %q", got.Status.SuspendedReason)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == condModelAccessScoped && c.Status == metav1.ConditionTrue {
			t.Errorf("ModelAccessScoped must not report True on a suspended Platform")
		}
	}
}
