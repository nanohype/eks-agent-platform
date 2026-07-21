/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// prodScope is the fully-resolved ARN scope used across the expansion tests —
// what a real reconcile derives from the tenant role ARN + operator config.
var prodScope = arnScope{Partition: "aws", Region: "us-west-2", AccountID: "123456789012"}

func TestExpandModelResources(t *testing.T) {
	cases := []struct {
		name     string
		identity platformv1alpha1.IdentitySpec
		scope    arnScope
		want     []string
		wantErr  string
	}{
		{
			name:     "empty spec expands to nil (deny-by-default upstream)",
			identity: platformv1alpha1.IdentitySpec{},
			scope:    prodScope,
			want:     nil,
		},
		{
			name:     "anthropic family: foundation-model + us. inference-profile patterns",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/anthropic.*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.anthropic.*",
			},
		},
		{
			name:     "amazon-nova family",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"amazon-nova"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/amazon.nova-*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.amazon.nova-*",
			},
		},
		{
			name:     "amazon-titan family has no cross-region profiles: foundation-model only",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"amazon-titan"}},
			scope:    prodScope,
			want:     []string{"arn:aws:bedrock:*::foundation-model/amazon.titan-*"},
		},
		{
			name:     "meta family",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"meta"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/meta.*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.meta.*",
			},
		},
		{
			name:     "mistral family",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"mistral"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/mistral.*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.mistral.*",
			},
		},
		{
			name:     "cohere family: foundation-model only",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"cohere"}},
			scope:    prodScope,
			want:     []string{"arn:aws:bedrock:*::foundation-model/cohere.*"},
		},
		{
			name:     "stability family: foundation-model only",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"stability"}},
			scope:    prodScope,
			want:     []string{"arn:aws:bedrock:*::foundation-model/stability.*"},
		},
		{
			name:     "multiple families merge sorted + deduped",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic", "amazon-titan", "anthropic"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/amazon.titan-*",
				"arn:aws:bedrock:*::foundation-model/anthropic.*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.anthropic.*",
			},
		},
		{
			name:     "unknown family errors instead of silently shipping deny-by-default",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"openai"}},
			scope:    prodScope,
			wantErr:  `unknown model family "openai"`,
		},
		{
			name:     "explicit bare model ID: its foundation-model ARN + implied us. profile",
			identity: platformv1alpha1.IdentitySpec{AllowedModels: []string{"anthropic.claude-sonnet-4-6"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/anthropic.claude-sonnet-4-6*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.anthropic.claude-sonnet-4-6*",
			},
		},
		{
			name:     "explicit inference-profile ID: profile ARN + underlying foundation-model ARN",
			identity: platformv1alpha1.IdentitySpec{AllowedModels: []string{"us.anthropic.claude-sonnet-4-6-v1:0"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/anthropic.claude-sonnet-4-6-v1:0*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.anthropic.claude-sonnet-4-6-v1:0*",
			},
		},
		{
			name: "explicit models take precedence over families when both slip past admission",
			identity: platformv1alpha1.IdentitySpec{
				AllowedModels:        []string{"amazon.titan-embed-text-v2"},
				AllowedModelFamilies: []string{"anthropic"},
			},
			scope: prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/amazon.titan-embed-text-v2*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.amazon.titan-embed-text-v2*",
			},
		},
		{
			name:     "entries already carrying a wildcard are not double-starred",
			identity: platformv1alpha1.IdentitySpec{AllowedModels: []string{"anthropic.claude-*"}},
			scope:    prodScope,
			want: []string{
				"arn:aws:bedrock:*::foundation-model/anthropic.claude-*",
				"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.anthropic.claude-*",
			},
		},
		{
			name:     "blank entries are skipped",
			identity: platformv1alpha1.IdentitySpec{AllowedModels: []string{"  ", ""}},
			scope:    prodScope,
			want:     nil,
		},
		{
			name:     "zero-value scope wildcards partition defaults, region, account",
			identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			scope:    arnScope{},
			want: []string{
				"arn:aws:bedrock:*:*:inference-profile/us.anthropic.*",
				"arn:aws:bedrock:*::foundation-model/anthropic.*",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandModelResources(tc.identity, tc.scope)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got %v want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandModelResources: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("resources:\n got  %v\n want %v", got, tc.want)
			}
		})
	}
}

func TestArnScopeFromRole(t *testing.T) {
	s := arnScopeFromRole("arn:aws-us-gov:iam::210987654321:role/eks-agent-platform/tenants/production-x-tenant", "us-gov-west-1")
	if s.Partition != "aws-us-gov" || s.AccountID != "210987654321" || s.Region != "us-gov-west-1" {
		t.Errorf("scope: got %+v", s)
	}
	// Garbage ARN degrades to wildcards, never panics.
	s = arnScopeFromRole("not-an-arn", "")
	if s.partition() != "aws" || s.region() != "*" || s.account() != "*" {
		t.Errorf("degraded scope: got partition=%s region=%s account=%s", s.partition(), s.region(), s.account())
	}
}

func TestModelScopingPolicyDoc(t *testing.T) {
	t.Run("empty resource set renders a deny-everything clamp", func(t *testing.T) {
		doc, err := modelScopingPolicyDoc(nil)
		if err != nil {
			t.Fatalf("modelScopingPolicyDoc: %v", err)
		}
		var parsed policyDocument
		if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(parsed.Statement) != 1 {
			t.Fatalf("statements: got %d want 1", len(parsed.Statement))
		}
		s := parsed.Statement[0]
		if s.Effect != "Deny" || s.Sid != "DenyAllBedrockInvoke" {
			t.Errorf("statement: got %+v", s)
		}
		if !reflect.DeepEqual(s.Resource, []string{"*"}) || len(s.NotResource) != 0 {
			t.Errorf("resources: got Resource=%v NotResource=%v", s.Resource, s.NotResource)
		}
		if !reflect.DeepEqual(s.Action, modelInvokeActions) {
			t.Errorf("actions: got %v", s.Action)
		}
		// The clamp must never catch guardrail actions — they authorize
		// against guardrail ARNs, not model ARNs.
		for _, a := range s.Action {
			if strings.Contains(a, "Guardrail") {
				t.Errorf("guardrail action %s must not be clamped", a)
			}
		}
	})

	t.Run("non-empty set denies everything outside it via NotResource", func(t *testing.T) {
		resources := []string{
			"arn:aws:bedrock:*::foundation-model/anthropic.*",
			"arn:aws:bedrock:us-west-2:123456789012:inference-profile/us.anthropic.*",
		}
		doc, err := modelScopingPolicyDoc(resources)
		if err != nil {
			t.Fatalf("modelScopingPolicyDoc: %v", err)
		}
		var parsed policyDocument
		if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		s := parsed.Statement[0]
		if s.Effect != "Deny" || s.Sid != "DenyUnscopedBedrockInvoke" {
			t.Errorf("statement: got %+v", s)
		}
		if !reflect.DeepEqual(s.NotResource, resources) || len(s.Resource) != 0 {
			t.Errorf("resources: got Resource=%v NotResource=%v", s.Resource, s.NotResource)
		}
	})
}

func TestEnsureIamRole_ReconcilesModelScopingPolicy(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	cfg := IAMConfig{
		TenantBaselinePolicyARN: baseline,
		ClusterName:             "production-cluster",
		Environment:             "production",
		Region:                  "us-west-2",
	}

	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	platform := newPlatform("slack-knowledge-bot", "workplace")
	platform.Spec.Identity.AllowedModelFamilies = []string{"anthropic"}
	roleName := tenantRoleName(cfg.ClusterName, platform)

	// Fresh role: the scoping policy lands alongside the baseline attach.
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	doc, ok := f.inline[roleName][modelScopingPolicyName]
	if !ok {
		t.Fatalf("expected inline policy %s on %s; inline=%v", modelScopingPolicyName, roleName, f.inline)
	}
	if !strings.Contains(doc, "foundation-model/anthropic.*") || !strings.Contains(doc, "inference-profile/us.anthropic.*") {
		t.Errorf("scoping doc missing anthropic patterns: %s", doc)
	}
	if !strings.Contains(doc, "DenyUnscopedBedrockInvoke") {
		t.Errorf("scoping doc missing the NotResource deny: %s", doc)
	}
	if len(f.putInlineCalls) != 1 {
		t.Fatalf("PutRolePolicy calls: got %d want 1", len(f.putInlineCalls))
	}

	// Idempotent: converged spec re-reconciles without a write.
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("second ensureIamRole: %v", err)
	}
	if len(f.putInlineCalls) != 1 {
		t.Errorf("PutRolePolicy calls after converged re-run: got %d want 1", len(f.putInlineCalls))
	}

	// Spec change: the document follows.
	platform.Spec.Identity.AllowedModelFamilies = nil
	platform.Spec.Identity.AllowedModels = []string{"amazon.titan-embed-text-v2"}
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("ensureIamRole after spec change: %v", err)
	}
	doc = f.inline[roleName][modelScopingPolicyName]
	if !strings.Contains(doc, "foundation-model/amazon.titan-embed-text-v2*") || strings.Contains(doc, "anthropic") {
		t.Errorf("scoping doc did not follow the spec change: %s", doc)
	}
	if len(f.putInlineCalls) != 2 {
		t.Errorf("PutRolePolicy calls after spec change: got %d want 2", len(f.putInlineCalls))
	}

	// Fields cleared: the grant is removed — the policy degrades to the
	// deny-everything clamp so the baseline's wildcard invoke stays
	// unreachable (deny-by-default).
	platform.Spec.Identity.AllowedModels = nil
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("ensureIamRole after clearing spec: %v", err)
	}
	doc = f.inline[roleName][modelScopingPolicyName]
	if !strings.Contains(doc, "DenyAllBedrockInvoke") || strings.Contains(doc, "NotResource") {
		t.Errorf("cleared spec must render the deny-all clamp: %s", doc)
	}
}

func TestEnsureIamRole_SuspendedDoesNotTouchModelScopingPolicy(t *testing.T) {
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
		Region:                  "us-west-2",
	}
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	platform := newPlatform("slack-knowledge-bot", "workplace")
	platform.Spec.Identity.AllowedModelFamilies = []string{"anthropic"}

	roleName := tenantRoleName(cfg.ClusterName, platform)
	f.seedRole(roleName, "arn:aws:iam::123:role/"+roleName,
		iamtypes.Tag{Key: aws.String(suspendedTag), Value: aws.String("true")},
	)

	got, err := r.ensureIamRole(context.Background(), platform, cfg)
	if err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	if !got.Suspended {
		t.Fatalf("expected Suspended=true")
	}
	if len(f.putInlineCalls) != 0 {
		t.Errorf("suspended role must not receive the model policy: got %d PutRolePolicy calls", len(f.putInlineCalls))
	}
	if len(f.deleteInlineCalls) != 0 {
		t.Errorf("operator is observe-only while suspended: got %d DeleteRolePolicy calls", len(f.deleteInlineCalls))
	}
}

func TestDeleteIamRole_RemovesInlinePoliciesBeforeRoleDelete(t *testing.T) {
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
	}
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	platform := newPlatform("slack-knowledge-bot", "workplace")
	platform.Spec.Identity.AllowedModelFamilies = []string{"anthropic"}

	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("ensureIamRole: %v", err)
	}
	roleName := tenantRoleName(cfg.ClusterName, platform)
	if _, ok := f.inline[roleName][modelScopingPolicyName]; !ok {
		t.Fatalf("precondition: scoping policy should exist")
	}

	if err := r.deleteIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("deleteIamRole: %v", err)
	}
	if len(f.inline[roleName]) != 0 {
		t.Errorf("inline policies must be deleted with the role: %v", f.inline[roleName])
	}
	if _, ok := f.roles[roleName]; ok {
		t.Errorf("role should be deleted")
	}

	// Finalizer re-run on the already-deleted role is a safe no-op.
	if err := r.deleteIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatalf("deleteIamRole re-run: %v", err)
	}
}

func TestEnsureSessionRole_CarriesModelScopingPolicy(t *testing.T) {
	cfg := IAMConfig{
		TenantBaselinePolicyARN: "arn:aws:iam::aws:policy/EksAgentBaseline",
		ClusterName:             "production-cluster",
		Environment:             "production",
		Region:                  "us-west-2",
	}
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	platform := newPlatform("slack-knowledge-bot", "workplace")
	platform.Spec.Identity.AllowedModelFamilies = []string{"anthropic"}
	platform.Spec.Attribution = &platformv1alpha1.AttributionSpec{Operators: []string{"ops@nanohype.dev"}}

	tenantARN := "arn:aws:iam::123456789012:role/production-slack-knowledge-bot-tenant"
	if _, err := r.ensureSessionRole(context.Background(), platform, tenantARN, false, cfg); err != nil {
		t.Fatalf("ensureSessionRole: %v", err)
	}
	sessionName := sessionRoleName(cfg.ClusterName, platform)
	doc, ok := f.inline[sessionName][modelScopingPolicyName]
	if !ok {
		t.Fatalf("session role must carry the model scoping clamp; inline=%v", f.inline)
	}
	if !strings.Contains(doc, "foundation-model/anthropic.*") {
		t.Errorf("session scoping doc missing anthropic patterns: %s", doc)
	}

	// Suspended: the session reconcile detaches the baseline and leaves the
	// scoping policy alone (no re-put).
	puts := len(f.putInlineCalls)
	if _, err := r.ensureSessionRole(context.Background(), platform, tenantARN, true, cfg); err != nil {
		t.Fatalf("ensureSessionRole (suspended): %v", err)
	}
	if len(f.putInlineCalls) != puts {
		t.Errorf("suspended session role must not receive policy writes: got %d new puts", len(f.putInlineCalls)-puts)
	}
}
