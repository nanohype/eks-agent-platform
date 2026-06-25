/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
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

// fakeIAM is a minimal in-memory implementation of awsclients.IAM used to
// cover policy-attachment reconcile logic without spinning up LocalStack.
// Only the methods the reconciler actually calls are populated; the rest
// panic so adding a new SDK call without test coverage is loud.
type fakeIAM struct {
	roles    map[string]*iamtypes.Role
	attached map[string]map[string]struct{} // roleName -> set of policy ARNs

	listCalls         int
	attachCalls       []iam.AttachRolePolicyInput
	createCalls       []iam.CreateRoleInput
	updateAssumeCalls []iam.UpdateAssumeRolePolicyInput
	detachCalls       []iam.DetachRolePolicyInput
	listReturnsErr    error
	attachReturnsErr  map[string]error // policyARN -> err
	pageBoundary      int              // if > 0, paginate ListAttached at this size
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{
		roles:            map[string]*iamtypes.Role{},
		attached:         map[string]map[string]struct{}{},
		attachReturnsErr: map[string]error{},
	}
}

func (f *fakeIAM) seedRole(name, arn string, tags ...iamtypes.Tag) {
	f.roles[name] = &iamtypes.Role{
		RoleName: aws.String(name),
		Arn:      aws.String(arn),
		Tags:     tags,
	}
	if _, ok := f.attached[name]; !ok {
		f.attached[name] = map[string]struct{}{}
	}
}

func (f *fakeIAM) seedAttachment(roleName, policyARN string) { //nolint:unparam // test fake seed helper
	if _, ok := f.attached[roleName]; !ok {
		f.attached[roleName] = map[string]struct{}{}
	}
	f.attached[roleName][policyARN] = struct{}{}
}

func (f *fakeIAM) attachmentsFor(roleName string) []string {
	out := make([]string, 0, len(f.attached[roleName]))
	for arn := range f.attached[roleName] {
		out = append(out, arn)
	}
	sort.Strings(out)
	return out
}

func (f *fakeIAM) CreateRole(_ context.Context, params *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.createCalls = append(f.createCalls, *params)
	name := aws.ToString(params.RoleName)
	arn := "arn:aws:iam::123456789012:role/" + name
	f.seedRole(name, arn, params.Tags...)
	return &iam.CreateRoleOutput{Role: f.roles[name]}, nil
}

func (f *fakeIAM) GetRole(_ context.Context, params *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	name := aws.ToString(params.RoleName)
	role, ok := f.roles[name]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no such role: " + name)}
	}
	return &iam.GetRoleOutput{Role: role}, nil
}

func (f *fakeIAM) DeleteRole(_ context.Context, params *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	name := aws.ToString(params.RoleName)
	delete(f.roles, name)
	delete(f.attached, name)
	return &iam.DeleteRoleOutput{}, nil
}

func (f *fakeIAM) TagRole(_ context.Context, _ *iam.TagRoleInput, _ ...func(*iam.Options)) (*iam.TagRoleOutput, error) {
	return &iam.TagRoleOutput{}, nil
}

func (f *fakeIAM) UpdateAssumeRolePolicy(_ context.Context, params *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	f.updateAssumeCalls = append(f.updateAssumeCalls, *params)
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

func (f *fakeIAM) AttachRolePolicy(_ context.Context, params *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	arn := aws.ToString(params.PolicyArn)
	if err, ok := f.attachReturnsErr[arn]; ok && err != nil {
		return nil, err
	}
	f.attachCalls = append(f.attachCalls, *params)
	roleName := aws.ToString(params.RoleName)
	if _, ok := f.attached[roleName]; !ok {
		f.attached[roleName] = map[string]struct{}{}
	}
	f.attached[roleName][arn] = struct{}{}
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *fakeIAM) DetachRolePolicy(_ context.Context, params *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.detachCalls = append(f.detachCalls, *params)
	roleName := aws.ToString(params.RoleName)
	delete(f.attached[roleName], aws.ToString(params.PolicyArn))
	return &iam.DetachRolePolicyOutput{}, nil
}

func (f *fakeIAM) ListAttachedRolePolicies(_ context.Context, params *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	f.listCalls++
	if f.listReturnsErr != nil {
		return nil, f.listReturnsErr
	}
	roleName := aws.ToString(params.RoleName)
	all := []iamtypes.AttachedPolicy{}
	for arn := range f.attached[roleName] {
		all = append(all, iamtypes.AttachedPolicy{
			PolicyArn:  aws.String(arn),
			PolicyName: aws.String(arn),
		})
	}
	sort.Slice(all, func(i, j int) bool {
		return aws.ToString(all[i].PolicyArn) < aws.ToString(all[j].PolicyArn)
	})
	if f.pageBoundary <= 0 || len(all) <= f.pageBoundary {
		return &iam.ListAttachedRolePoliciesOutput{AttachedPolicies: all}, nil
	}
	// Two-page pagination: first call returns first page + truncation marker;
	// second call (Marker non-nil) returns the rest.
	if params.Marker == nil {
		return &iam.ListAttachedRolePoliciesOutput{
			AttachedPolicies: all[:f.pageBoundary],
			IsTruncated:      true,
			Marker:           aws.String("page-2"),
		}, nil
	}
	return &iam.ListAttachedRolePoliciesOutput{
		AttachedPolicies: all[f.pageBoundary:],
	}, nil
}

func TestReconcileManagedPolicies(t *testing.T) {
	const role = "production-acme-tenant"
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	extras := []string{
		"arn:aws:iam::123:policy/slack-knowledge-bot-tenant",
		"arn:aws:iam::123:policy/digest-pipeline-tenant",
	}

	t.Run("fresh role: attaches baseline + every extra", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}

		got := f.attachmentsFor(role)
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
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.seedAttachment(role, baseline)
		f.seedAttachment(role, extras[0])
		f.seedAttachment(role, extras[1])
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		if len(f.attachCalls) != 0 {
			t.Errorf("attach calls: got %d want 0 (no diff)", len(f.attachCalls))
		}
	})

	t.Run("partial state: only attaches the missing entries", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.seedAttachment(role, baseline)  // already there
		f.seedAttachment(role, extras[0]) // already there
		// extras[1] missing — should be the only attach
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		if len(f.attachCalls) != 1 {
			t.Fatalf("attach calls: got %d want 1", len(f.attachCalls))
		}
		if aws.ToString(f.attachCalls[0].PolicyArn) != extras[1] {
			t.Errorf("attached the wrong policy: got %s want %s", aws.ToString(f.attachCalls[0].PolicyArn), extras[1])
		}
	})

	t.Run("does NOT detach attachments that aren't in the desired set", func(t *testing.T) {
		// Operator team may have attached a policy out-of-band (e.g.
		// emergency break-glass). The reconciler must not fight them.
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.seedAttachment(role, "arn:aws:iam::123:policy/operator-break-glass") // foreign
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, nil); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		got := f.attachmentsFor(role)
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
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, "", nil); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		if f.listCalls != 0 {
			t.Errorf("list calls: got %d want 0 (should short-circuit)", f.listCalls)
		}
		if len(f.attachCalls) != 0 {
			t.Errorf("attach calls: got %d want 0", len(f.attachCalls))
		}
	})

	t.Run("empty strings in extras are filtered before attach", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, []string{"", extras[0], ""}); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		got := f.attachmentsFor(role)
		want := []string{baseline, extras[0]}
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("attachments: got %v want %v", got, want)
		}
	})

	t.Run("paginates the attachment list", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		// 5 pre-existing attachments + page boundary of 2 → 3 pages
		for i, arn := range []string{
			"arn:aws:iam::123:policy/x1",
			"arn:aws:iam::123:policy/x2",
			"arn:aws:iam::123:policy/x3",
			baseline,
			extras[0],
		} {
			_ = i
			f.seedAttachment(role, arn)
		}
		f.pageBoundary = 2
		r := &PlatformReconciler{IAM: f}

		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, extras); err != nil {
			t.Fatalf("reconcileManagedPolicies: %v", err)
		}
		// baseline + extras[0] already attached; only extras[1] should
		// be attached after the paginated scan.
		if len(f.attachCalls) != 1 {
			t.Fatalf("attach calls: got %d want 1", len(f.attachCalls))
		}
		if aws.ToString(f.attachCalls[0].PolicyArn) != extras[1] {
			t.Errorf("attached the wrong policy: got %s", aws.ToString(f.attachCalls[0].PolicyArn))
		}
	})
}

func TestEnsureIamRole_AttachesExtraPolicyArns(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	extras := []string{
		"arn:aws:iam::123:policy/slack-knowledge-bot-tenant",
		"arn:aws:iam::123:policy/slack-knowledge-bot-tenant-bedrock",
	}

	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: baseline,
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	platform := newPlatform("slack-knowledge-bot", "protohype")
	platform.Spec.Identity.ExtraPolicyArns = extras

	got, err := r.ensureIamRole(context.Background(), platform, cfg)
	if err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	if got.RoleARN == "" {
		t.Errorf("expected RoleARN to be set")
	}

	roleName := tenantRoleName(cfg.Environment, platform)
	attachments := f.attachmentsFor(roleName)
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

	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: baseline,
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	platform := newPlatform("slack-knowledge-bot", "protohype")
	platform.Spec.Identity.ExtraPolicyArns = extras

	// Seed the role with the suspension marker so ensureIamRole's GetRole
	// branch returns suspended=true and skips reconcileManagedPolicies.
	roleName := tenantRoleName(cfg.Environment, platform)
	f.seedRole(roleName, "arn:aws:iam::123:role/"+roleName,
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
	if len(f.attachCalls) != 0 {
		t.Errorf("expected no attach calls when suspended, got %d", len(f.attachCalls))
	}
}

// fakeEKS is a minimal in-memory awsclients.EKS covering the Pod Identity
// association the operator binds the tenant ServiceAccount with.
type fakeEKS struct {
	associations map[string]string // "namespace/serviceAccount" -> association id
	createCalls  []eks.CreatePodIdentityAssociationInput
	deleteCalls  []eks.DeletePodIdentityAssociationInput
	nextID       int
}

func newFakeEKS() *fakeEKS {
	return &fakeEKS{associations: map[string]string{}}
}

func (f *fakeEKS) ListPodIdentityAssociations(_ context.Context, params *eks.ListPodIdentityAssociationsInput, _ ...func(*eks.Options)) (*eks.ListPodIdentityAssociationsOutput, error) {
	key := aws.ToString(params.Namespace) + "/" + aws.ToString(params.ServiceAccount)
	out := &eks.ListPodIdentityAssociationsOutput{}
	if id, ok := f.associations[key]; ok {
		out.Associations = []ekstypes.PodIdentityAssociationSummary{{
			AssociationId:  aws.String(id),
			Namespace:      params.Namespace,
			ServiceAccount: params.ServiceAccount,
		}}
	}
	return out, nil
}

func (f *fakeEKS) CreatePodIdentityAssociation(_ context.Context, params *eks.CreatePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.CreatePodIdentityAssociationOutput, error) {
	f.createCalls = append(f.createCalls, *params)
	f.nextID++
	id := fmt.Sprintf("a-%d", f.nextID)
	f.associations[aws.ToString(params.Namespace)+"/"+aws.ToString(params.ServiceAccount)] = id
	return &eks.CreatePodIdentityAssociationOutput{}, nil
}

func (f *fakeEKS) DeletePodIdentityAssociation(_ context.Context, params *eks.DeletePodIdentityAssociationInput, _ ...func(*eks.Options)) (*eks.DeletePodIdentityAssociationOutput, error) {
	f.deleteCalls = append(f.deleteCalls, *params)
	for k, id := range f.associations {
		if id == aws.ToString(params.AssociationId) {
			delete(f.associations, k)
		}
	}
	return &eks.DeletePodIdentityAssociationOutput{}, nil
}

func TestEnsureIamRole_CreatesPodIdentityAssociation(t *testing.T) {
	f := newFakeIAM()
	fe := newFakeEKS()
	r := &PlatformReconciler{IAM: f, EKS: fe}
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	platform := newPlatform("slack-knowledge-bot", "protohype")

	got, err := r.ensureIamRole(context.Background(), platform, cfg)
	if err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}

	// The role trusts the EKS Pod Identity service principal, not an OIDC provider.
	if len(f.createCalls) != 1 {
		t.Fatalf("expected one CreateRole, got %d", len(f.createCalls))
	}
	trust := aws.ToString(f.createCalls[0].AssumeRolePolicyDocument)
	if !strings.Contains(trust, "pods.eks.amazonaws.com") {
		t.Errorf("trust policy missing pods.eks.amazonaws.com principal: %s", trust)
	}
	if strings.Contains(trust, "AssumeRoleWithWebIdentity") {
		t.Errorf("trust policy must not use the IRSA web-identity action: %s", trust)
	}

	// A Pod Identity association binds the tenant SA (tenant-runtime) to the role.
	if len(fe.createCalls) != 1 {
		t.Fatalf("expected one CreatePodIdentityAssociation, got %d", len(fe.createCalls))
	}
	assoc := fe.createCalls[0]
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

	// Idempotent: a second reconcile finds the existing association, no duplicate.
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("second ensureIamRole: %v", err)
	}
	if len(fe.createCalls) != 1 {
		t.Errorf("association creation not idempotent: got %d creates", len(fe.createCalls))
	}

	// deleteIamRole removes the association before the role.
	if err := r.deleteIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("deleteIamRole: %v", err)
	}
	if len(fe.deleteCalls) != 1 {
		t.Errorf("expected one DeletePodIdentityAssociation, got %d", len(fe.deleteCalls))
	}
}

func TestEnsurePodIdentityAssociation_RequiresClusterName(t *testing.T) {
	r := &PlatformReconciler{EKS: newFakeEKS()}
	err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{}, "tenants-x", tenantSAName, "arn:aws:iam::123:role/x")
	if err == nil {
		t.Fatal("expected an error when ClusterName is empty and the EKS client is wired")
	}
	if !strings.Contains(err.Error(), "ClusterName") {
		t.Errorf("error should name the missing ClusterName: %v", err)
	}
}

func TestDeletePodIdentityAssociation_ToleratesMissing(t *testing.T) {
	fe := newFakeEKS()
	r := &PlatformReconciler{EKS: fe}
	// No association seeded — the finalizer delete must be a safe no-op so re-runs
	// (and Platforms whose association was never created) don't error.
	if err := r.deletePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "production-cluster"}, "tenants-x", tenantSAName); err != nil {
		t.Fatalf("delete with no association should be a no-op: %v", err)
	}
	if len(fe.deleteCalls) != 0 {
		t.Errorf("expected no DeletePodIdentityAssociation calls, got %d", len(fe.deleteCalls))
	}
}
