/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// TestAssumeRolePolicyForPodIdentity pins the trust policy every tenant role is
// minted with. Nothing asserted it — the document is emitted once at role
// creation and never read back, so no test observed any field of it.
//
// Both actions are load-bearing and the failure is neither loud nor local:
//
//	sts:AssumeRole   without it the association exists and no pod ever gets
//	                 credentials
//	sts:TagSession   EKS Pod Identity attaches session tags on every assume, so
//	                 without it the assume is rejected — pods come up, the
//	                 Platform reports Ready, and every AWS call fails with an
//	                 authorization error naming a session tag rather than the
//	                 trust policy
//
// The principal is what scopes it to Pod Identity at all: pods.eks.amazonaws.com
// is a different service principal from ec2 or eks, and naming the wrong one
// produces a role nothing can assume.
func TestAssumeRolePolicyForPodIdentity(t *testing.T) {
	raw, err := assumeRolePolicyForPodIdentity()
	if err != nil {
		t.Fatalf("assumeRolePolicyForPodIdentity: %v", err)
	}
	var doc struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string            `json:"Effect"`
			Principal map[string]string `json:"Principal"`
			Action    []string          `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("trust policy is not valid JSON: %v", err)
	}
	if doc.Version != "2012-10-17" {
		t.Errorf("Version = %q, want 2012-10-17", doc.Version)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("want one statement, got %d", len(doc.Statement))
	}
	s := doc.Statement[0]
	if s.Effect != "Allow" {
		t.Errorf("Effect = %q, want Allow", s.Effect)
	}
	if got := s.Principal["Service"]; got != "pods.eks.amazonaws.com" {
		t.Errorf("principal service %q, want pods.eks.amazonaws.com — any other principal mints a role "+
			"Pod Identity cannot assume", got)
	}
	want := []string{"sts:AssumeRole", "sts:TagSession"}
	if !reflect.DeepEqual(s.Action, want) {
		t.Errorf("actions = %v, want %v — Pod Identity tags every session, so dropping sts:TagSession "+
			"leaves pods running with a Ready Platform and every AWS call rejected", s.Action, want)
	}
}

func TestSuspensionFromTags(t *testing.T) {
	cases := []struct {
		name       string
		tags       []iamtypes.Tag
		wantSusp   bool
		wantReason string
	}{
		{name: "empty", tags: nil, wantSusp: false, wantReason: ""},
		{name: "no_marker", tags: []iamtypes.Tag{
			{Key: aws.String("Environment"), Value: aws.String("production")},
			{Key: aws.String("PlatformId"), Value: aws.String("acme")},
		}, wantSusp: false, wantReason: ""},
		{name: "suspended_true", tags: []iamtypes.Tag{
			{Key: aws.String(suspendedTag), Value: aws.String("true")},
			{Key: aws.String(suspendedReasonTag), Value: aws.String("budget-exceeded")},
		}, wantSusp: true, wantReason: "budget-exceeded"},
		{name: "suspended_false_string", tags: []iamtypes.Tag{
			{Key: aws.String(suspendedTag), Value: aws.String("false")},
		}, wantSusp: false, wantReason: ""},
		{name: "suspended_true_no_reason", tags: []iamtypes.Tag{
			{Key: aws.String(suspendedTag), Value: aws.String("true")},
		}, wantSusp: true, wantReason: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotSusp, gotReason := suspensionFromTags(c.tags)
			if gotSusp != c.wantSusp {
				t.Errorf("suspended: got %v want %v", gotSusp, c.wantSusp)
			}
			if gotReason != c.wantReason {
				t.Errorf("reason: got %q want %q", gotReason, c.wantReason)
			}
		})
	}
}

func tagMap(tags []iamtypes.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func TestTenantRoleTags(t *testing.T) {
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec:       platformv1alpha1.PlatformSpec{Tenant: "acme-team", Persona: "founder"},
	}

	// Empty org-dim config: the required keys must still be present (defaults).
	// ClusterName is NOT left empty — it qualifies the PlatformId cost identity,
	// and an empty one renders "-acme", which is why main.go refuses to start
	// without it. TestCostIdentity_* owns the identity's correctness; this test
	// owns the tag SET, so it supplies a realistic cluster and asserts the value
	// through the same constructor the reconciler queries with.
	got := tagMap(tenantRoleTags(p, IAMConfig{Environment: "production", ClusterName: "production-platform"}))

	// The required-tier resource-tagging keys cloudgov gates on, plus the
	// load-bearing PlatformId / Tenant / Persona the rest of the system reads.
	for _, k := range []string{
		"Environment", "ManagedBy", "Project", "Repository", "Component", "Team",
		"CostCenter", "BusinessUnit", "DataClassification", "Compliance",
		"PlatformId", "Tenant", "Persona",
	} {
		if got[k] == "" {
			t.Errorf("tenantRoleTags missing/empty key %q (have %v)", k, got)
		}
	}
	if want := platformCostID("production-platform", ctrlTestPlatform); got["PlatformId"] != want {
		t.Errorf("PlatformId: got %q want %q", got["PlatformId"], want)
	}
	if got["ManagedBy"] != "eks-agent-platform" {
		t.Errorf("ManagedBy: got %q want eks-agent-platform", got["ManagedBy"])
	}
	if got["CostCenter"] != "platform-engineering" {
		t.Errorf("CostCenter default: got %q want platform-engineering", got["CostCenter"])
	}

	// Explicit config wins over the defaults.
	got = tagMap(tenantRoleTags(p, IAMConfig{
		Environment: "development", CostCenter: "research", BusinessUnit: "labs",
		DataClassification: "confidential", Compliance: "hipaa",
	}))
	if got["CostCenter"] != "research" || got["Compliance"] != "hipaa" {
		t.Errorf("config override not applied: %v", got)
	}
}
