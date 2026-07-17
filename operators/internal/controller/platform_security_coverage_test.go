/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	smithy "github.com/aws/smithy-go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// These tests drive the reachable error and configuration branches on the three
// security-critical reconcilers — the tenant/session IAM role and the KMS-grant
// + S3 bucket-policy paths that ARE the tenant isolation boundary — to the
// per-file 100% floor coverage-check enforces. Each AWS call's failure path is
// exercised through the fakes' error-injection hooks, so a regression that drops
// an error check (or a test that stops covering one) fails the gate.

var errBoom = errors.New("boom")

// codeNoSuchEntity is IAM's not-found error code.
const codeNoSuchEntity = "NoSuchEntity"

// apiErr builds a smithy API error carrying an arbitrary code — used to prove
// the NotFound-tolerant paths distinguish a specific code from a hard failure.
func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

// fakeCtrlClient builds an empty controller-runtime client on a scheme with the
// kinds the vcluster synced-SA discovery lists. Empty ⇒ discovery returns
// errVClusterNotReady, which is what the vcluster IAM branch propagates.
func fakeCtrlClient(t *testing.T) *PlatformReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return &PlatformReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
}

// ── platform_iam.go ──────────────────────────────────────────────────────────

func TestTenantRoleName_HashTruncatesOverLimit(t *testing.T) {
	long := ""
	for i := 0; i < 80; i++ {
		long += "a"
	}
	p := newPlatform(long, "acme")
	name := tenantRoleName("production-cluster", p)
	if len(name) > 64 {
		t.Errorf("tenant role name over IAM's 64-char limit: %d (%s)", len(name), name)
	}
	if name[len(name)-7:] != "-tenant" {
		t.Errorf("truncated name must keep the -tenant suffix: %s", name)
	}
}

func TestEnsureIamRole_NormalizesPathWithoutTrailingSlash(t *testing.T) {
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{TenantIAMPath: "eks-agent-platform/custom", ClusterName: "c", Environment: "e"}
	if _, err := r.ensureIamRole(context.Background(), newPlatform("acme", "t"), cfg); err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	if got := aws.ToString(f.createCalls[0].Path); got != "eks-agent-platform/custom/" {
		t.Errorf("path not normalized with a trailing slash: %q", got)
	}
}

func TestEnsureIamRole_GetRoleHardErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.getReturnsErr = errBoom
	r := &PlatformReconciler{IAM: f}
	if _, err := r.ensureIamRole(context.Background(), newPlatform("acme", "t"), IAMConfig{ClusterName: "c"}); err == nil {
		t.Fatal("a non-NotFound GetRole error must abort ensureIamRole")
	}
}

func TestEnsureIamRole_SetsPermissionsBoundaryOnCreate(t *testing.T) {
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{TenantPermissionsBoundaryARN: "arn:aws:iam::123:policy/boundary", ClusterName: "c", Environment: "e"}
	if _, err := r.ensureIamRole(context.Background(), newPlatform("acme", "t"), cfg); err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	if aws.ToString(f.createCalls[0].PermissionsBoundary) != cfg.TenantPermissionsBoundaryARN {
		t.Error("the permissions boundary must be set on the created tenant role")
	}
}

func TestEnsureIamRole_CreateRoleErrorPropagates(t *testing.T) {
	f := newFakeIAM()
	f.createReturnsErr = errBoom
	r := &PlatformReconciler{IAM: f}
	if _, err := r.ensureIamRole(context.Background(), newPlatform("acme", "t"), IAMConfig{ClusterName: "c"}); err == nil {
		t.Fatal("a CreateRole error must abort ensureIamRole")
	}
}

func TestEnsureIamRole_ExistingRoleReconcileErrors(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	cfg := IAMConfig{TenantBaselinePolicyARN: baseline, ClusterName: "c", Environment: "e"}
	p := newPlatform("acme", "t")
	p.Spec.Identity.AllowedModelFamilies = []string{"anthropic"}
	name := tenantRoleName(cfg.ClusterName, p)

	t.Run("managed-policy reconcile error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(name, "arn:aws:iam::123:role/"+name)
		f.listReturnsErr = errBoom // ListAttachedRolePolicies fails
		r := &PlatformReconciler{IAM: f}
		if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
			t.Fatal("expected the managed-policy reconcile error to propagate")
		}
	})

	t.Run("model-scoping error on unknown family", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(name, "arn:aws:iam::123:role/"+name)
		f.seedAttachment(name, baseline) // so managed-policy reconcile is a no-op
		bad := newPlatform("acme", "t")
		bad.Spec.Identity.AllowedModelFamilies = []string{"nonsense-family"}
		r := &PlatformReconciler{IAM: f}
		if _, err := r.ensureIamRole(context.Background(), bad, cfg); err == nil {
			t.Fatal("an unknown model family must fail the model-scoping reconcile")
		}
	})

	t.Run("pod-identity error via unready vcluster", func(t *testing.T) {
		r := fakeCtrlClient(t)
		f := newFakeIAM()
		f.seedRole(name, "arn:aws:iam::123:role/"+name)
		f.seedAttachment(name, baseline)
		r.IAM = f
		vp := newPlatform("acme", "t")
		vp.Spec.Identity.AllowedModelFamilies = []string{"anthropic"}
		vp.Spec.Isolation = isolationVCluster
		if _, err := r.ensureIamRole(context.Background(), vp, cfg); !errors.Is(err, errVClusterNotReady) {
			t.Fatalf("expected errVClusterNotReady from the synced-SA discovery, got %v", err)
		}
	})
}

func TestEnsureIamRole_FreshRoleReconcileErrors(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	cfg := IAMConfig{TenantBaselinePolicyARN: baseline, ClusterName: "c", Environment: "e"}

	t.Run("managed-policy reconcile error after create", func(t *testing.T) {
		f := newFakeIAM()
		f.listReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		p := newPlatform("acme", "t")
		if _, err := r.ensureIamRole(context.Background(), p, cfg); err == nil {
			t.Fatal("expected the post-create managed-policy reconcile error to propagate")
		}
	})

	t.Run("model-scoping error after create", func(t *testing.T) {
		f := newFakeIAM()
		r := &PlatformReconciler{IAM: f}
		p := newPlatform("acme", "t")
		p.Spec.Identity.AllowedModelFamilies = []string{"nonsense-family"}
		if _, err := r.ensureIamRole(context.Background(), p, IAMConfig{ClusterName: "c", Environment: "e"}); err == nil {
			t.Fatal("an unknown model family must fail the fresh-role model-scoping reconcile")
		}
	})

	t.Run("pod-identity error after create via unready vcluster", func(t *testing.T) {
		r := fakeCtrlClient(t)
		r.IAM = newFakeIAM()
		vp := newPlatform("acme", "t")
		vp.Spec.Isolation = isolationVCluster
		if _, err := r.ensureIamRole(context.Background(), vp, IAMConfig{ClusterName: "c", Environment: "e"}); !errors.Is(err, errVClusterNotReady) {
			t.Fatalf("expected errVClusterNotReady, got %v", err)
		}
	})
}

func TestSecurityReconcilers_NilClientNoOps(t *testing.T) {
	ctx := context.Background()
	p := newPlatform("acme", "t")

	// ensureIamRole with no IAM client (envtest / dev without AWS creds) is a
	// silent no-op returning an empty suspension.
	r := &PlatformReconciler{}
	if got, err := r.ensureIamRole(ctx, p, IAMConfig{}); err != nil || got.RoleARN != "" {
		t.Errorf("nil IAM ensureIamRole: got (%+v, %v) want (empty, nil)", got, err)
	}
	// detachAndDeleteRole with no IAM client is a no-op.
	if err := r.detachAndDeleteRole(ctx, "any"); err != nil {
		t.Errorf("nil IAM detachAndDeleteRole: %v", err)
	}
	// ensureBucketPolicy / removeBucketPolicyStatements with no S3 client no-op.
	if err := r.ensureBucketPolicy(ctx, p, "role", PlatformAWSConfig{ArtifactsBucketName: "b"}); err != nil {
		t.Errorf("nil S3 ensureBucketPolicy: %v", err)
	}
	if err := r.removeBucketPolicyStatements(ctx, p, PlatformAWSConfig{ArtifactsBucketName: "b"}); err != nil {
		t.Errorf("nil S3 removeBucketPolicyStatements: %v", err)
	}
}

func TestEnsurePodIdentityAssociation_Errors(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		fe := newFakeEKS()
		fe.listReturnsErr = errBoom
		r := &PlatformReconciler{EKS: fe}
		if err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", tenantSAName, "arn"); err == nil {
			t.Fatal("expected the ListPodIdentityAssociations error to propagate")
		}
	})
	t.Run("create error", func(t *testing.T) {
		fe := newFakeEKS()
		fe.createReturnsErr = errBoom
		r := &PlatformReconciler{EKS: fe}
		if err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", tenantSAName, "arn"); err == nil {
			t.Fatal("expected the CreatePodIdentityAssociation error to propagate")
		}
	})
}

func TestDeletePodIdentityAssociation_Errors(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		fe := newFakeEKS()
		fe.listReturnsErr = errBoom
		r := &PlatformReconciler{EKS: fe}
		if err := r.deletePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", tenantSAName); err == nil {
			t.Fatal("expected the list error to propagate on the delete path")
		}
	})
	t.Run("delete error", func(t *testing.T) {
		fe := newFakeEKS()
		fe.associations["ns/"+tenantSAName] = "a-1"
		fe.deleteReturnsErr = errBoom
		r := &PlatformReconciler{EKS: fe}
		if err := r.deletePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", tenantSAName); err == nil {
			t.Fatal("expected the DeletePodIdentityAssociation error to propagate")
		}
	})
}

func TestReconcileManagedPolicies_Errors(t *testing.T) {
	const role = "r"
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	t.Run("list error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.listReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, nil); err == nil {
			t.Fatal("expected the ListAttachedRolePolicies error to propagate")
		}
	})
	t.Run("attach error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.attachReturnsErr[baseline] = errBoom
		r := &PlatformReconciler{IAM: f}
		if err := r.reconcileManagedPolicies(context.Background(), role, baseline, nil); err == nil {
			t.Fatal("expected the AttachRolePolicy error to propagate")
		}
	})
}

func TestDeleteIamRole_PodIdentityErrorPropagates(t *testing.T) {
	fe := newFakeEKS()
	fe.listReturnsErr = errBoom
	r := &PlatformReconciler{IAM: newFakeIAM(), EKS: fe}
	if err := r.deleteIamRole(context.Background(), newPlatform("acme", "t"), IAMConfig{ClusterName: "c"}); err == nil {
		t.Fatal("a Pod Identity teardown error must abort the finalizer before deleting the role")
	}
}

func TestDetachAndDeleteRole_Branches(t *testing.T) {
	const role = "production-acme-tenant"

	t.Run("absent role NotFound is a clean no-op", func(t *testing.T) {
		f := newFakeIAM() // no role seeded ⇒ ListAttached returns NoSuchEntity
		f.listReturnsErr = apiErr(codeNoSuchEntity)
		r := &PlatformReconciler{IAM: f}
		if err := r.detachAndDeleteRole(context.Background(), role); err != nil {
			t.Fatalf("NotFound on a never-created role must be tolerated: %v", err)
		}
	})
	t.Run("list hard error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.listReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if err := r.detachAndDeleteRole(context.Background(), role); err == nil {
			t.Fatal("a non-NotFound list error must propagate")
		}
	})
	t.Run("detach error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.seedAttachment(role, "arn:aws:iam::123:policy/x")
		f.detachReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if err := r.detachAndDeleteRole(context.Background(), role); err == nil {
			t.Fatal("a DetachRolePolicy error must propagate")
		}
	})
	t.Run("paginates the attachment list on teardown", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.seedAttachment(role, "arn:aws:iam::123:policy/a")
		f.seedAttachment(role, "arn:aws:iam::123:policy/b")
		f.pageBoundary = 1
		r := &PlatformReconciler{IAM: f}
		if err := r.detachAndDeleteRole(context.Background(), role); err != nil {
			t.Fatalf("paginated teardown: %v", err)
		}
		if _, ok := f.roles[role]; ok {
			t.Error("role should be deleted after a paginated detach")
		}
	})
	t.Run("inline-policy delete error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.listInlineReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if err := r.detachAndDeleteRole(context.Background(), role); err == nil {
			t.Fatal("an inline-policy listing error must propagate")
		}
	})
	t.Run("delete-role error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(role, "arn:aws:iam::123:role/"+role)
		f.deleteReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if err := r.detachAndDeleteRole(context.Background(), role); err == nil {
			t.Fatal("a DeleteRole error must propagate")
		}
	})
}

func TestIsIAMNotFound(t *testing.T) {
	if isIAMNotFound(nil) {
		t.Error("nil is not a NotFound")
	}
	if isIAMNotFound(errBoom) {
		t.Error("a plain error is not an IAM NoSuchEntity")
	}
	if isIAMNotFound(apiErr("SomethingElse")) {
		t.Error("a different API code is not NoSuchEntity")
	}
	if !isIAMNotFound(apiErr(codeNoSuchEntity)) {
		t.Error("NoSuchEntity must be recognized")
	}
}

// ── platform_session_iam.go ──────────────────────────────────────────────────

func TestSessionRoleName_HashTruncatesOverLimit(t *testing.T) {
	long := ""
	for i := 0; i < 80; i++ {
		long += "b"
	}
	name := sessionRoleName("production-cluster", newPlatform(long, "t"))
	if len(name) > 64 {
		t.Errorf("session role name over IAM's 64-char limit: %d", len(name))
	}
	if name[len(name)-8:] != "-session" {
		t.Errorf("truncated name must keep the -session suffix: %s", name)
	}
}

func TestEnsureSessionRole_ExistingRoleErrors(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	cfg := IAMConfig{TenantBaselinePolicyARN: baseline, ClusterName: "c", Environment: "e"}
	p := attributedPlatform("acme", "t", []string{"a@x.com"}, nil)
	name := sessionRoleName(cfg.ClusterName, p)

	t.Run("update-trust error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(name, "arn:aws:iam::123:role/"+name)
		f.updateAssumeErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if _, err := r.ensureSessionRole(context.Background(), p, "arn:tenant", false, cfg); err == nil {
			t.Fatal("an UpdateAssumeRolePolicy error must propagate")
		}
	})
	t.Run("baseline reconcile error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(name, "arn:aws:iam::123:role/"+name)
		f.listReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if _, err := r.ensureSessionRole(context.Background(), p, "arn:tenant", false, cfg); err == nil {
			t.Fatal("a baseline reconcile error must propagate")
		}
	})
	t.Run("model-scoping error", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(name, "arn:aws:iam::123:role/"+name)
		f.seedAttachment(name, baseline)
		bad := attributedPlatform("acme", "t", []string{"a@x.com"}, nil)
		bad.Spec.Identity.AllowedModelFamilies = []string{"nonsense-family"}
		r := &PlatformReconciler{IAM: f}
		if _, err := r.ensureSessionRole(context.Background(), bad, "arn:tenant", false, cfg); err == nil {
			t.Fatal("an unknown family must fail the session model-scoping reconcile")
		}
	})
	t.Run("get hard error", func(t *testing.T) {
		f := newFakeIAM()
		f.getReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		if _, err := r.ensureSessionRole(context.Background(), p, "arn:tenant", false, cfg); err == nil {
			t.Fatal("a non-NotFound GetRole error must propagate")
		}
	})
}

func TestEnsureSessionRole_FreshRoleErrors(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"

	t.Run("normalizes path and sets boundary", func(t *testing.T) {
		f := newFakeIAM()
		r := &PlatformReconciler{IAM: f}
		cfg := IAMConfig{TenantIAMPath: "custom/p", TenantPermissionsBoundaryARN: "arn:boundary", ClusterName: "c", Environment: "e"}
		p := attributedPlatform("acme", "t", []string{"a@x.com"}, nil)
		if _, err := r.ensureSessionRole(context.Background(), p, "arn:tenant", false, cfg); err != nil {
			t.Fatalf("ensureSessionRole: %v", err)
		}
		if aws.ToString(f.createCalls[0].Path) != "custom/p/" {
			t.Errorf("session-role path not normalized: %q", aws.ToString(f.createCalls[0].Path))
		}
		if aws.ToString(f.createCalls[0].PermissionsBoundary) != "arn:boundary" {
			t.Error("session role must carry the permissions boundary")
		}
	})
	t.Run("create error", func(t *testing.T) {
		f := newFakeIAM()
		f.createReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		p := attributedPlatform("acme", "t", []string{"a@x.com"}, nil)
		if _, err := r.ensureSessionRole(context.Background(), p, "arn:tenant", false, IAMConfig{ClusterName: "c"}); err == nil {
			t.Fatal("a CreateRole error must propagate")
		}
	})
	t.Run("baseline reconcile error after create", func(t *testing.T) {
		f := newFakeIAM()
		f.listReturnsErr = errBoom
		r := &PlatformReconciler{IAM: f}
		p := attributedPlatform("acme", "t", []string{"a@x.com"}, nil)
		cfg := IAMConfig{TenantBaselinePolicyARN: baseline, ClusterName: "c", Environment: "e"}
		if _, err := r.ensureSessionRole(context.Background(), p, "arn:tenant", false, cfg); err == nil {
			t.Fatal("a post-create baseline reconcile error must propagate")
		}
	})
	t.Run("model-scoping error after create", func(t *testing.T) {
		f := newFakeIAM()
		r := &PlatformReconciler{IAM: f}
		p := attributedPlatform("acme", "t", []string{"a@x.com"}, nil)
		p.Spec.Identity.AllowedModelFamilies = []string{"nonsense-family"}
		if _, err := r.ensureSessionRole(context.Background(), p, "arn:tenant", false, IAMConfig{ClusterName: "c", Environment: "e"}); err == nil {
			t.Fatal("an unknown family must fail the fresh session-role model-scoping reconcile")
		}
	})
}

func TestReconcileSessionBaseline_SuspendedDetachError(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	f := newFakeIAM()
	name := "s"
	f.seedRole(name, "arn:aws:iam::123:role/"+name)
	f.seedAttachment(name, baseline)
	f.detachReturnsErr = errBoom
	r := &PlatformReconciler{IAM: f}
	if err := r.reconcileSessionBaseline(context.Background(), name, baseline, true); err == nil {
		t.Fatal("a DetachRolePolicy error on the suspend path must propagate")
	}
}

// ── platform_kms_s3.go ───────────────────────────────────────────────────────

func TestEnsureKmsGrant_Errors(t *testing.T) {
	cfg := PlatformAWSConfig{DataKMSKeyARN: "arn:aws:kms:us-west-2:123:key/abc"}
	t.Run("list error", func(t *testing.T) {
		k := &fakeKMS{listReturnsErr: errBoom}
		r := &PlatformReconciler{KMS: k}
		if err := r.ensureKmsGrant(context.Background(), newPlatform("acme", "t"), "role", cfg); err == nil {
			t.Fatal("a ListGrants error must propagate")
		}
	})
	t.Run("create error", func(t *testing.T) {
		k := &fakeKMS{createReturnsErr: errBoom}
		r := &PlatformReconciler{KMS: k}
		if err := r.ensureKmsGrant(context.Background(), newPlatform("acme", "t"), "role", cfg); err == nil {
			t.Fatal("a CreateGrant error must propagate")
		}
	})
	t.Run("nil client is a no-op", func(t *testing.T) {
		r := &PlatformReconciler{}
		if err := r.ensureKmsGrant(context.Background(), newPlatform("acme", "t"), "role", cfg); err != nil {
			t.Fatalf("nil KMS client must be a silent no-op: %v", err)
		}
	})
}

func TestRevokeKmsGrant(t *testing.T) {
	cfg := PlatformAWSConfig{DataKMSKeyARN: "arn:aws:kms:us-west-2:123:key/abc"}
	p := newPlatform("acme", "t")

	t.Run("revokes the matching grant, ignores others", func(t *testing.T) {
		k := &fakeKMS{grants: []kmstypes.GrantListEntry{
			{Name: aws.String("tenant-other"), GrantId: aws.String("g0")},
			{Name: aws.String("tenant-acme"), GrantId: aws.String("g1")},
		}}
		r := &PlatformReconciler{KMS: k}
		if err := r.revokeKmsGrant(context.Background(), p, cfg); err != nil {
			t.Fatalf("revokeKmsGrant: %v", err)
		}
		if len(k.revoked) != 1 || k.revoked[0] != "g1" {
			t.Errorf("expected only the tenant-acme grant revoked, got %v", k.revoked)
		}
	})
	t.Run("finds the grant across pages", func(t *testing.T) {
		k := &fakeKMS{
			grants: []kmstypes.GrantListEntry{
				{Name: aws.String("tenant-other"), GrantId: aws.String("g0")},
				{Name: aws.String("tenant-acme"), GrantId: aws.String("g1")},
			},
			pageBoundary: 1,
		}
		r := &PlatformReconciler{KMS: k}
		if err := r.revokeKmsGrant(context.Background(), p, cfg); err != nil {
			t.Fatalf("revokeKmsGrant paginated: %v", err)
		}
		if len(k.revoked) != 1 {
			t.Errorf("paginated revoke: got %v", k.revoked)
		}
	})
	t.Run("list error", func(t *testing.T) {
		k := &fakeKMS{listReturnsErr: errBoom}
		r := &PlatformReconciler{KMS: k}
		if err := r.revokeKmsGrant(context.Background(), p, cfg); err == nil {
			t.Fatal("a ListGrants error on the revoke path must propagate")
		}
	})
	t.Run("revoke hard error", func(t *testing.T) {
		k := &fakeKMS{
			grants:           []kmstypes.GrantListEntry{{Name: aws.String("tenant-acme"), GrantId: aws.String("g1")}},
			revokeReturnsErr: errBoom,
		}
		r := &PlatformReconciler{KMS: k}
		if err := r.revokeKmsGrant(context.Background(), p, cfg); err == nil {
			t.Fatal("a non-NotFound RevokeGrant error must propagate")
		}
	})
	t.Run("revoke NotFound is tolerated", func(t *testing.T) {
		k := &fakeKMS{
			grants:           []kmstypes.GrantListEntry{{Name: aws.String("tenant-acme"), GrantId: aws.String("g1")}},
			revokeReturnsErr: apiErr("NotFoundException"),
		}
		r := &PlatformReconciler{KMS: k}
		if err := r.revokeKmsGrant(context.Background(), p, cfg); err != nil {
			t.Fatalf("a grant already gone must be a tolerated no-op: %v", err)
		}
	})
	t.Run("nil client is a no-op", func(t *testing.T) {
		r := &PlatformReconciler{}
		if err := r.revokeKmsGrant(context.Background(), p, cfg); err != nil {
			t.Fatalf("nil KMS revoke must be a no-op: %v", err)
		}
	})
}

func TestEnsureBucketPolicy_Errors(t *testing.T) {
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}
	p := newPlatform("acme", "t")
	t.Run("fetch error", func(t *testing.T) {
		s := &fakeS3{getReturnsErr: errBoom}
		r := &PlatformReconciler{S3: s}
		if err := r.ensureBucketPolicy(context.Background(), p, "role-arn", cfg); err == nil {
			t.Fatal("a non-NoSuchBucketPolicy GetBucketPolicy error must propagate")
		}
	})
	t.Run("put error", func(t *testing.T) {
		s := &fakeS3{putReturnsErr: errBoom}
		r := &PlatformReconciler{S3: s}
		if err := r.ensureBucketPolicy(context.Background(), p, "role-arn", cfg); err == nil {
			t.Fatal("a PutBucketPolicy error must propagate")
		}
	})
}

func TestRemoveBucketPolicyStatements_Branches(t *testing.T) {
	cfg := PlatformAWSConfig{ArtifactsBucketName: "artifacts"}
	p := newPlatform("acme", "t")
	t.Run("fetch error", func(t *testing.T) {
		s := &fakeS3{getReturnsErr: errBoom}
		r := &PlatformReconciler{S3: s}
		if err := r.removeBucketPolicyStatements(context.Background(), p, cfg); err == nil {
			t.Fatal("a fetch error must propagate on the finalizer path")
		}
	})
	t.Run("no statements is a no-op", func(t *testing.T) {
		s := &fakeS3{policy: aws.String(`{"Version":"2012-10-17"}`)}
		r := &PlatformReconciler{S3: s}
		if err := r.removeBucketPolicyStatements(context.Background(), p, cfg); err != nil {
			t.Fatalf("a policy with no statements must be a clean no-op: %v", err)
		}
		if len(s.puts) != 0 || len(s.deletes) != 0 {
			t.Error("no write should happen when there are no statements to remove")
		}
	})
	t.Run("nothing owned is a no-op", func(t *testing.T) {
		s := &fakeS3{policy: aws.String(`{"Version":"2012-10-17","Statement":[{"Sid":"TenantAccess-other"}]}`)}
		r := &PlatformReconciler{S3: s}
		if err := r.removeBucketPolicyStatements(context.Background(), p, cfg); err != nil {
			t.Fatalf("removeBucketPolicyStatements: %v", err)
		}
		if len(s.puts) != 0 || len(s.deletes) != 0 {
			t.Error("no write should happen when this Platform owns no statements")
		}
	})
	t.Run("delete-policy error when last statement removed", func(t *testing.T) {
		s := &fakeS3{
			policy:           aws.String(`{"Version":"2012-10-17","Statement":[{"Sid":"TenantAccess-acme"},{"Sid":"TenantAccess-acme-List"}]}`),
			deleteReturnsErr: errBoom,
		}
		r := &PlatformReconciler{S3: s}
		if err := r.removeBucketPolicyStatements(context.Background(), p, cfg); err == nil {
			t.Fatal("a DeleteBucketPolicy error must propagate")
		}
	})
	t.Run("put error when peer statements remain", func(t *testing.T) {
		s := &fakeS3{
			policy:        aws.String(`{"Version":"2012-10-17","Statement":[{"Sid":"TenantAccess-acme"},{"Sid":"TenantAccess-other"}]}`),
			putReturnsErr: errBoom,
		}
		r := &PlatformReconciler{S3: s}
		if err := r.removeBucketPolicyStatements(context.Background(), p, cfg); err == nil {
			t.Fatal("a PutBucketPolicy error on the finalizer rewrite must propagate")
		}
	})
}

func TestFetchBucketPolicy_Branches(t *testing.T) {
	r := &PlatformReconciler{}
	t.Run("malformed JSON errors", func(t *testing.T) {
		r.S3 = &fakeS3{policy: aws.String("{not valid json")}
		if _, err := r.fetchBucketPolicy(context.Background(), "b"); err == nil {
			t.Fatal("a malformed bucket policy must surface as a parse error")
		}
	})
	t.Run("null policy yields an empty doc", func(t *testing.T) {
		r.S3 = &fakeS3{policy: aws.String("null")}
		doc, err := r.fetchBucketPolicy(context.Background(), "b")
		if err != nil {
			t.Fatalf("fetchBucketPolicy: %v", err)
		}
		if doc == nil || len(doc) != 0 {
			t.Errorf("a null policy must decode to an empty (non-nil) doc, got %v", doc)
		}
	})
}

func TestIsAPIErrorCode(t *testing.T) {
	if isAPIErrorCode(nil, "X") {
		t.Error("nil is never an API error code")
	}
	if isAPIErrorCode(errBoom, "X") {
		t.Error("a plain error carries no API code")
	}
	if !isAPIErrorCode(apiErr("NoSuchBucketPolicy"), "NoSuchBucketPolicy") {
		t.Error("a matching API code must be recognized")
	}
}
