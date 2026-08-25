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

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// platform_model_scoping.go generates the explicit-Deny document that is the
// Bedrock model-authorization boundary, so it carries the coverage gate's
// per-file 100% override alongside the other policy generators
// (hack/coverage-check/main.go fileFloors). These are the branches the
// behavioural tests in platform_model_scoping_test.go do not reach: the failure
// paths, and the two condition renderers whose output is the only thing telling
// an operator which of the two scoping modes a Platform ended up in.

func TestModelScopeConditionReason(t *testing.T) {
	cases := []struct {
		name     string
		identity platformv1alpha1.IdentitySpec
		want     string
	}{
		{
			// The one that has to be unambiguous: an empty identity is the
			// deny-everything clamp, and a reason reading "Scoped" there would
			// describe a Platform that can invoke nothing as one that was
			// narrowed to something.
			name:     "no declaration reports deny-by-default",
			identity: platformv1alpha1.IdentitySpec{},
			want:     "DenyByDefault",
		},
		{
			name:     "explicit models report scoped",
			identity: platformv1alpha1.IdentitySpec{AllowedModels: []string{"anthropic.claude-sonnet-4-6"}},
			want:     "Scoped",
		},
		{
			name:     "families report scoped",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			want:     "Scoped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelScopeConditionReason(tc.identity); got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelScopeConditionMessage(t *testing.T) {
	cases := []struct {
		name     string
		identity platformv1alpha1.IdentitySpec
		want     string
	}{
		{
			name:     "no declaration says everything is denied",
			identity: platformv1alpha1.IdentitySpec{},
			want:     "no allowed models declared; all bedrock invoke denied",
		},
		{
			name: "models are named individually",
			identity: platformv1alpha1.IdentitySpec{
				AllowedModels: []string{"anthropic.claude-sonnet-4-6", "amazon.nova-lite-v1:0"},
			},
			want: "bedrock invoke scoped to models [anthropic.claude-sonnet-4-6, amazon.nova-lite-v1:0]",
		},
		{
			// Models win when both are set, matching expandModelResources —
			// a message naming the families while the policy scoped to the
			// models would send an auditor to the wrong document.
			name: "models take precedence over families",
			identity: platformv1alpha1.IdentitySpec{
				AllowedModels:        []string{"anthropic.claude-sonnet-4-6"},
				AllowedModelFamilies: []string{"anthropic"},
			},
			want: "bedrock invoke scoped to models [anthropic.claude-sonnet-4-6]",
		},
		{
			name:     "families are named when models are absent",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic", "meta"}},
			want:     "bedrock invoke scoped to model families [anthropic, meta]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelScopeConditionMessage(tc.identity); got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpandModelResourcesSkipsBlankFamilyEntries(t *testing.T) {
	// A blank entry is dropped rather than expanded. Expanding it would produce
	// the family prefix ARN for the empty family — foundation-model/* — which
	// lands in NotResource and excludes every model from the Deny.
	got, err := expandModelResources(platformv1alpha1.IdentitySpec{
		AllowedModelFamilies: []string{"anthropic", "", "   "},
	}, prodScope)
	if err != nil {
		t.Fatalf("expandModelResources: %v", err)
	}
	for _, arn := range got {
		if strings.HasSuffix(arn, "foundation-model/*") || strings.HasSuffix(arn, "inference-profile/us.*") {
			t.Errorf("a blank family entry expanded to the catch-all %q, which would empty the Deny", arn)
		}
	}
	if len(got) == 0 {
		t.Error("the real family alongside the blanks expanded to nothing")
	}
}

func TestEnsureModelScopingPolicyErrorBranches(t *testing.T) {
	ctx := context.Background()
	cfg := IAMConfig{Region: "us-west-2"}
	const roleName = "development-platform-app-tenant"
	const roleARN = "arn:aws:iam::123456789012:role/" + roleName
	scoped := platformv1alpha1.IdentitySpec{AllowedModels: []string{"anthropic.claude-sonnet-4-6"}}

	t.Run("no IAM client is a no-op, not a failure", func(t *testing.T) {
		// envtest and dev clusters without IRSA run the reconciler with no AWS
		// wiring; the k8s half must still converge.
		r := &PlatformReconciler{}
		if err := r.ensureModelScopingPolicy(ctx, roleName, roleARN, scoped, cfg); err != nil {
			t.Errorf("nil IAM client: %v", err)
		}
	})

	t.Run("an unknown model family fails the reconcile", func(t *testing.T) {
		r := &PlatformReconciler{IAM: newFakeIAM()}
		err := r.ensureModelScopingPolicy(ctx, roleName, roleARN,
			platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"nonesuch"}}, cfg)
		if err == nil || !strings.Contains(err.Error(), "expand allowed models") {
			t.Errorf("unknown family: got %v, want an expand failure", err)
		}
	})

	t.Run("a hard GetRolePolicy error is returned, not treated as absent", func(t *testing.T) {
		// NoSuchEntity means "write it"; anything else means the current
		// document is unknown, and writing over an unknown document could
		// replace a narrower policy with a wider one.
		f := newFakeIAM()
		f.seedRole(roleName, roleARN)
		f.getInlineReturnsErr = map[string]error{modelScopingPolicyName: errors.New("throttled")}
		r := &PlatformReconciler{IAM: f}
		err := r.ensureModelScopingPolicy(ctx, roleName, roleARN, scoped, cfg)
		if err == nil || !strings.Contains(err.Error(), "GetRolePolicy") {
			t.Errorf("hard read error: got %v, want it surfaced", err)
		}
		if len(f.putInlineCalls) != 0 {
			t.Error("a failed read still wrote the policy")
		}
	})

	t.Run("a PutRolePolicy failure is returned", func(t *testing.T) {
		f := newFakeIAM()
		f.seedRole(roleName, roleARN)
		f.putInlineReturnsErr = map[string]error{modelScopingPolicyName: errors.New("limit exceeded")}
		r := &PlatformReconciler{IAM: f}
		err := r.ensureModelScopingPolicy(ctx, roleName, roleARN, scoped, cfg)
		if err == nil || !strings.Contains(err.Error(), "PutRolePolicy") {
			t.Errorf("write error: got %v, want it surfaced", err)
		}
	})
}

func TestPolicyDocEqualTreatsUnparseableAsDrift(t *testing.T) {
	// Returning false on an unparseable document makes the reconciler rewrite
	// it. That is the safe direction: the alternative is treating a document it
	// cannot read as already correct and leaving whatever is there in place.
	valid := `{"Version":"2012-10-17","Statement":[]}`
	if policyDocEqual("{not json", valid) {
		t.Error("an unparseable CURRENT document compared equal, so drift would never be corrected")
	}
	if policyDocEqual(valid, "{not json") {
		t.Error("an unparseable DESIRED document compared equal")
	}
}

func TestDeleteInlinePoliciesSurfacesADeleteFailure(t *testing.T) {
	// The finalizer path. A delete that fails silently leaves an inline policy
	// on a role the operator is about to try to delete, and role deletion fails
	// with a message about the policy rather than about the delete.
	const roleName = "development-platform-app-tenant"
	f := newFakeIAM()
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.inline[roleName] = map[string]string{modelScopingPolicyName: `{"Version":"2012-10-17"}`}
	f.deleteInlineReturnsErr = map[string]error{modelScopingPolicyName: errors.New("throttled")}

	r := &PlatformReconciler{IAM: f}
	err := r.deleteInlinePolicies(context.Background(), roleName)
	if err == nil || !strings.Contains(err.Error(), "DeleteRolePolicy") {
		t.Errorf("delete failure: got %v, want it surfaced", err)
	}
}

func TestDeleteInlinePoliciesFollowsPagination(t *testing.T) {
	// ListRolePolicies paginates, and a loop that ignores the marker deletes
	// only the first page — leaving inline policies on a role whose Platform is
	// gone, which is a grant outliving the thing that justified it.
	const roleName = "development-platform-app-tenant"
	f := newFakeIAM()
	f.seedRole(roleName, "arn:aws:iam::123456789012:role/"+roleName)
	f.inline[roleName] = map[string]string{
		modelScopingPolicyName: `{"Version":"2012-10-17"}`,
		"datastore-access":     `{"Version":"2012-10-17"}`,
		"capability-access":    `{"Version":"2012-10-17"}`,
	}
	f.inlinePageSize = 1

	r := &PlatformReconciler{IAM: f}
	if err := r.deleteInlinePolicies(context.Background(), roleName); err != nil {
		t.Fatalf("deleteInlinePolicies: %v", err)
	}
	if left := len(f.inline[roleName]); left != 0 {
		t.Errorf("%d inline policies survived a paginated delete: %v", left, f.inline[roleName])
	}
}
