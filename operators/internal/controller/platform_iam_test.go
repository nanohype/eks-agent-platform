/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

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
	got := tagMap(tenantRoleTags(p, IAMConfig{Environment: "production"}))

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
	if got["PlatformId"] != "acme" {
		t.Errorf("PlatformId: got %q want acme", got["PlatformId"])
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
