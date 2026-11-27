/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

func newPlatform(name, tenant string) *platformv1alpha1.Platform {
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "eks-agent-platform",
		},
		Spec: platformv1alpha1.PlatformSpec{
			Tenant:  tenant,
			Persona: "generic",
		},
	}
}

func TestReconcileManagedPolicies(t *testing.T) {
	const role = "production-acme-tenant"
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	extras := []string{
		"arn:aws:iam::123:policy/slack-knowledge-bot-tenant",
		"arn:aws:iam::123:policy/digest-pipeline-tenant",
	}

	t.Run("fresh role: attaches baseline + every extra", func(t *testing.T) {
		f := awsfake.NewIAM()
		f.SeedRole(role, "arn:aws:iam::123:role/"+role)
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}

		got := f.AttachmentsFor(role)
		want := []string{baseline, extras[0], extras[1]}
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("attachments: got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("attachment[%d]: got %s want %s", i, got[i], want[i])
			}
		}
	})

	t.Run("idempotent: re-run is a no-op when baseline + extras are already attached", func(t *testing.T) {
		f := awsfake.NewIAM()
		f.SeedRole(role, "arn:aws:iam::123:role/"+role)
		f.SeedAttachment(role, baseline)
		f.SeedAttachment(role, extras[0])
		f.SeedAttachment(role, extras[1])
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		if len(f.AttachCalls) != 0 {
			t.Errorf("attach calls: got %d want 0 (no diff)", len(f.AttachCalls))
		}
	})

	t.Run("partial state: only attaches the missing entries", func(t *testing.T) {
		f := awsfake.NewIAM()
		f.SeedRole(role, "arn:aws:iam::123:role/"+role)
		f.SeedAttachment(role, baseline)  // already there
		f.SeedAttachment(role, extras[0]) // already there
		// extras[1] missing — should be the only attach
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		if len(f.AttachCalls) != 1 {
			t.Fatalf("attach calls: got %d want 1", len(f.AttachCalls))
		}
		if aws.ToString(f.AttachCalls[0].PolicyArn) != extras[1] {
			t.Errorf("attached the wrong policy: got %s want %s", aws.ToString(f.AttachCalls[0].PolicyArn), extras[1])
		}
	})

	t.Run("does NOT detach attachments that aren't in the desired set", func(t *testing.T) {
		// Operator team may have attached a policy out-of-band (e.g.
		// emergency break-glass). The reconciler must not fight them.
		f := awsfake.NewIAM()
		f.SeedRole(role, "arn:aws:iam::123:role/"+role)
		f.SeedAttachment(role, "arn:aws:iam::123:policy/operator-break-glass") // foreign
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, nil); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		got := f.AttachmentsFor(role)
		want := []string{"arn:aws:iam::123:policy/operator-break-glass", baseline}
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("attachments: got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("attachment[%d]: got %s want %s", i, got[i], want[i])
			}
		}
	})

	t.Run("empty baseline + empty extras: no-op (dev/test mode)", func(t *testing.T) {
		f := awsfake.NewIAM()
		f.SeedRole(role, "arn:aws:iam::123:role/"+role)
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, "", nil); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		if f.ListCalls != 0 {
			t.Errorf("list calls: got %d want 0 (should short-circuit)", f.ListCalls)
		}
		if len(f.AttachCalls) != 0 {
			t.Errorf("attach calls: got %d want 0", len(f.AttachCalls))
		}
	})

	t.Run("empty strings in extras are filtered before attach", func(t *testing.T) {
		f := awsfake.NewIAM()
		f.SeedRole(role, "arn:aws:iam::123:role/"+role)
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, []string{"", extras[0], ""}); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		got := f.AttachmentsFor(role)
		want := []string{baseline, extras[0]}
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("attachments: got %v want %v", got, want)
		}
	})

	t.Run("paginates the attachment list", func(t *testing.T) {
		f := awsfake.NewIAM()
		f.SeedRole(role, "arn:aws:iam::123:role/"+role)
		// 5 pre-existing attachments + page boundary of 2 → 3 pages
		for i, arn := range []string{
			"arn:aws:iam::123:policy/x1",
			"arn:aws:iam::123:policy/x2",
			"arn:aws:iam::123:policy/x3",
			baseline,
			extras[0],
		} {
			_ = i
			f.SeedAttachment(role, arn)
		}
		f.PageBoundary = 2
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		// baseline + extras[0] already attached; only extras[1] should
		// be attached after the paginated scan.
		if len(f.AttachCalls) != 1 {
			t.Fatalf("attach calls: got %d want 1", len(f.AttachCalls))
		}
		if aws.ToString(f.AttachCalls[0].PolicyArn) != extras[1] {
			t.Errorf("attached the wrong policy: got %s", aws.ToString(f.AttachCalls[0].PolicyArn))
		}
	})
}

func TestEnsureIamRole_AttachesExtraPolicyArns(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	extras := []string{
		"arn:aws:iam::123:policy/slack-knowledge-bot-tenant",
		"arn:aws:iam::123:policy/slack-knowledge-bot-tenant-bedrock",
	}

	f := awsfake.NewIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: baseline,
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	platform := newPlatform("slack-knowledge-bot", "workplace")
	platform.Spec.Identity.ExtraPolicyArns = extras

	got, err := r.ensureIamRole(context.Background(), platform, cfg)
	if err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	if got.RoleARN == "" {
		t.Errorf("expected RoleARN to be set")
	}

	roleName := tenantRoleName(cfg.ClusterName, platform)
	attachments := f.AttachmentsFor(roleName)
	wantSet := map[string]bool{baseline: true, extras[0]: true, extras[1]: true}
	if len(attachments) != len(wantSet) {
		t.Fatalf("attachments: got %v want %v", attachments, wantSet)
	}
	for _, arn := range attachments {
		if !wantSet[arn] {
			t.Errorf("unexpected attachment: %s", arn)
		}
	}
}

func TestEnsureIamRole_SkipsAttachmentsWhenSuspended(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	extras := []string{"arn:aws:iam::123:policy/slack-knowledge-bot-tenant"}

	f := awsfake.NewIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: baseline,
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	platform := newPlatform("slack-knowledge-bot", "workplace")
	platform.Spec.Identity.ExtraPolicyArns = extras

	// Seed the role with the suspension marker so ensureIamRole's GetRole
	// branch returns suspended=true and skips reconcileManagedPolicies.
	roleName := tenantRoleName(cfg.ClusterName, platform)
	f.SeedRole(roleName, "arn:aws:iam::123:role/"+roleName,
		iamtypes.Tag{Key: aws.String(suspendedTag), Value: aws.String("true")},
		iamtypes.Tag{Key: aws.String(suspendedReasonTag), Value: aws.String("budget-exceeded")},
	)

	got, err := r.ensureIamRole(context.Background(), platform, cfg)
	if err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	if !got.Suspended {
		t.Errorf("expected Suspended=true")
	}
	if got.Reason != "budget-exceeded" {
		t.Errorf("Reason: got %q want budget-exceeded", got.Reason)
	}
	if len(f.AttachCalls) != 0 {
		t.Errorf("expected no attach calls when suspended, got %d", len(f.AttachCalls))
	}
}

func TestEnsureIamRole_CreatesPodIdentityAssociation(t *testing.T) {
	f := awsfake.NewIAM()
	fe := awsfake.NewEKS()
	r := &PlatformReconciler{IAM: f, EKS: fe}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	platform := newPlatform("slack-knowledge-bot", "workplace")

	got, err := r.ensureIamRole(context.Background(), platform, cfg)
	if err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}

	// The role trusts the EKS Pod Identity service principal, not an OIDC provider.
	if len(f.CreateCalls) != 1 {
		t.Fatalf("expected one CreateRole, got %d", len(f.CreateCalls))
	}
	trust := aws.ToString(f.CreateCalls[0].AssumeRolePolicyDocument)
	if !strings.Contains(trust, "pods.eks.amazonaws.com") {
		t.Errorf("trust policy missing pods.eks.amazonaws.com principal: %s", trust)
	}
	if strings.Contains(trust, "AssumeRoleWithWebIdentity") {
		t.Errorf("trust policy must not use the IRSA web-identity action: %s", trust)
	}

	// A Pod Identity association binds the tenant SA (tenant-runtime) to the
	// role, and a second binds the ModelGateway's Envoy SA to the same role.
	if len(fe.CreateCalls) != 2 {
		t.Fatalf("expected two CreatePodIdentityAssociation calls (tenant-runtime, gateway), got %d", len(fe.CreateCalls))
	}
	assoc := fe.CreateCalls[0]
	if aws.ToString(assoc.ClusterName) != "production-cluster" {
		t.Errorf("association cluster: got %q", aws.ToString(assoc.ClusterName))
	}
	if aws.ToString(assoc.ServiceAccount) != tenantSAName {
		t.Errorf("association SA: got %q want %q", aws.ToString(assoc.ServiceAccount), tenantSAName)
	}
	if aws.ToString(assoc.Namespace) != PlatformNamespace(platform) {
		t.Errorf("association namespace: got %q want %q", aws.ToString(assoc.Namespace), PlatformNamespace(platform))
	}
	if aws.ToString(assoc.RoleArn) != got.RoleARN {
		t.Errorf("association role: got %q want %q", aws.ToString(assoc.RoleArn), got.RoleARN)
	}

	// The binding is published back, field for field, so a reader never has to
	// re-derive it from the role name or reimplement the SA naming.
	if got.PodIdentity == nil {
		t.Fatal("ensureIamRole reconciled an association but reported no binding")
	}
	want := platformv1alpha1.PodIdentityStatus{
		ClusterName:    aws.ToString(assoc.ClusterName),
		Namespace:      aws.ToString(assoc.Namespace),
		ServiceAccount: aws.ToString(assoc.ServiceAccount),
		RoleArn:        aws.ToString(assoc.RoleArn),
	}
	if *got.PodIdentity != want {
		t.Errorf("published binding does not match the association created:\n got %+v\nwant %+v", *got.PodIdentity, want)
	}

	gw := fe.CreateCalls[1]
	wantGW := platformv1alpha1.PodIdentityStatus{
		ClusterName:    "production-cluster",
		Namespace:      PlatformNamespace(platform),
		ServiceAccount: platform.Name + "-gateway",
		RoleArn:        got.RoleARN,
	}
	gotGW := platformv1alpha1.PodIdentityStatus{
		ClusterName:    aws.ToString(gw.ClusterName),
		Namespace:      aws.ToString(gw.Namespace),
		ServiceAccount: aws.ToString(gw.ServiceAccount),
		RoleArn:        aws.ToString(gw.RoleArn),
	}
	if gotGW != wantGW {
		t.Errorf("gateway association:\n got %+v\nwant %+v", gotGW, wantGW)
	}
	if got.GatewayPodIdentity == nil || *got.GatewayPodIdentity != wantGW {
		t.Errorf("published gateway binding: got %+v want %+v", got.GatewayPodIdentity, wantGW)
	}

	// Idempotent: a second reconcile finds the existing associations, no duplicate.
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("second ensureIamRole: %v", err)
	}
	if len(fe.CreateCalls) != 2 {
		t.Errorf("association creation not idempotent: got %d creates", len(fe.CreateCalls))
	}

	// deleteIamRole removes both associations before the role, so neither
	// outlives the role it names.
	if err := r.deleteIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("deleteIamRole: %v", err)
	}
	if len(fe.DeleteCalls) != 2 {
		t.Errorf("expected two DeletePodIdentityAssociation calls, got %d", len(fe.DeleteCalls))
	}
	if len(fe.Associations) != 0 {
		t.Errorf("associations survived deleteIamRole: %v", fe.Associations)
	}
}

// TestEnsureIamRole_NoEKSClient_PublishesNoBinding pins the fail-closed shape of
// status.podIdentity. Without an EKS client the operator creates no association,
// so it must report no binding — publishing a hollow one (right namespace and SA,
// no association behind it) would tell a reader the tenant is bound when nothing
// vends it credentials.
func TestEnsureIamRole_NoEKSClient_PublishesNoBinding(t *testing.T) {
	r := &PlatformReconciler{IAM: awsfake.NewIAM()}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
	}

	got, err := r.ensureIamRole(context.Background(), newPlatform("slack-knowledge-bot", "workplace"), cfg)
	if err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	if got.RoleARN == "" {
		t.Fatal("expected the role to be created on the nil-EKS path")
	}
	if got.PodIdentity != nil {
		t.Errorf("no association was created, so no binding should be published: %+v", *got.PodIdentity)
	}
}

// TestEnsureIamRole_AssociationFailure_PublishesNoBinding is the other half of
// fail-closed: when the association cannot be created the reconcile must error
// out with no binding published, rather than reporting a binding for an
// association that does not exist. An EKS client with no ClusterName is the
// cheapest way to make the association fail.
func TestEnsureIamRole_AssociationFailure_PublishesNoBinding(t *testing.T) {
	r := &PlatformReconciler{IAM: awsfake.NewIAM(), EKS: awsfake.NewEKS()}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		Environment:             "production",
	}

	got, err := r.ensureIamRole(context.Background(), newPlatform("slack-knowledge-bot", "workplace"), cfg)
	if err == nil {
		t.Fatal("expected ensureIamRole to fail when the association cannot be created")
	}
	if got.PodIdentity != nil {
		t.Errorf("failed association must publish no binding: %+v", *got.PodIdentity)
	}
}

func TestEnsurePodIdentityAssociation_RequiresClusterName(t *testing.T) {
	r := &PlatformReconciler{EKS: awsfake.NewEKS()}
	err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{}, "tenants-x", tenantSAName, "arn:aws:iam::123:role/x")
	if err == nil {
		t.Fatal("expected an error when ClusterName is empty and the EKS client is wired")
	}
	if !strings.Contains(err.Error(), "ClusterName") {
		t.Errorf("error should name the missing ClusterName: %v", err)
	}
}

func TestDeletePodIdentityAssociation_ToleratesMissing(t *testing.T) {
	fe := awsfake.NewEKS()
	r := &PlatformReconciler{EKS: fe}
	// No association seeded — the finalizer delete must be a safe no-op so re-runs
	// (and Platforms whose association was never created) don't error.
	if err := r.deletePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "production-cluster"}, "tenants-x", tenantSAName); err != nil {
		t.Fatalf("delete with no association should be a no-op: %v", err)
	}
	if len(fe.DeleteCalls) != 0 {
		t.Errorf("expected no DeletePodIdentityAssociation calls, got %d", len(fe.DeleteCalls))
	}
}
