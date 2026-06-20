/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// attributedPlatform builds a Platform with spec.attribution set. Shared with
// the RBAC tests (same package).
//
//nolint:unparam // test helper: name/tenant are fixed across cases by design
func attributedPlatform(name, tenant string, operators []string, maxDur *int32) *platformv1alpha1.Platform {
	p := newPlatform(name, tenant)
	p.Spec.Attribution = &platformv1alpha1.AttributionSpec{
		Operators:                     operators,
		SessionRoleMaxDurationSeconds: maxDur,
	}
	return p
}

func TestEnsureSessionRole_CreatesRoleWithTrustAndBaseline(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	const tenantARN = "arn:aws:iam::123456789012:role/production-acme-tenant"
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{TenantBaselinePolicyARN: baseline, Environment: "production"}
	p := attributedPlatform("acme", "protohype", []string{"alice@acme.com", "bob@acme.com"}, nil)

	arn, err := r.ensureSessionRole(context.Background(), p, tenantARN, false, cfg)
	if err != nil {
		t.Fatalf("ensureSessionRole: %v", err)
	}
	if arn == "" {
		t.Fatal("expected a session role ARN")
	}
	name := sessionRoleName(cfg.Environment, p)
	if name != "production-acme-session" {
		t.Fatalf("session role name: got %s want production-acme-session", name)
	}
	if len(f.createCalls) != 1 {
		t.Fatalf("create calls: got %d want 1", len(f.createCalls))
	}

	// Trust: only the tenant role may assume, only while setting one of the
	// operators as SourceIdentity, and NOT via web identity / broad assume.
	trust := aws.ToString(f.createCalls[0].AssumeRolePolicyDocument)
	for _, want := range []string{tenantARN, "sts:AssumeRole", "sts:SetSourceIdentity", "sts:SourceIdentity", "alice@acme.com", "bob@acme.com"} {
		if !strings.Contains(trust, want) {
			t.Errorf("trust policy missing %q:\n%s", want, trust)
		}
	}
	if strings.Contains(trust, "AssumeRoleWithWebIdentity") {
		t.Errorf("session role trust must not grant web-identity assume:\n%s", trust)
	}
	if got := aws.ToInt32(f.createCalls[0].MaxSessionDuration); got != 3600 {
		t.Errorf("MaxSessionDuration: got %d want 3600", got)
	}
	if got := f.attachmentsFor(name); len(got) != 1 || got[0] != baseline {
		t.Errorf("baseline attachment: got %v want [%s]", got, baseline)
	}
}

func TestEnsureSessionRole_CustomMaxDuration(t *testing.T) {
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{Environment: "production"}
	dur := int32(7200)
	p := attributedPlatform("acme", "protohype", []string{"alice@acme.com"}, &dur)

	if _, err := r.ensureSessionRole(context.Background(), p, "arn:aws:iam::1:role/tenant", false, cfg); err != nil {
		t.Fatalf("ensureSessionRole: %v", err)
	}
	if got := aws.ToInt32(f.createCalls[0].MaxSessionDuration); got != 7200 {
		t.Errorf("MaxSessionDuration: got %d want 7200", got)
	}
}

func TestEnsureSessionRole_IdempotentRefreshesTrust(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{TenantBaselinePolicyARN: baseline, Environment: "production"}
	p := attributedPlatform("acme", "protohype", []string{"alice@acme.com"}, nil)
	name := sessionRoleName(cfg.Environment, p)
	f.seedRole(name, "arn:aws:iam::123:role/"+name)

	if _, err := r.ensureSessionRole(context.Background(), p, "arn:aws:iam::1:role/tenant", false, cfg); err != nil {
		t.Fatalf("ensureSessionRole: %v", err)
	}
	if len(f.createCalls) != 0 {
		t.Errorf("create calls: got %d want 0 (role already existed)", len(f.createCalls))
	}
	if len(f.updateAssumeCalls) != 1 {
		t.Fatalf("trust-refresh calls: got %d want 1", len(f.updateAssumeCalls))
	}
	if !strings.Contains(aws.ToString(f.updateAssumeCalls[0].PolicyDocument), "alice@acme.com") {
		t.Errorf("refreshed trust should carry the operator")
	}
	if got := f.attachmentsFor(name); len(got) != 1 || got[0] != baseline {
		t.Errorf("baseline attachment: got %v", got)
	}
}

func TestEnsureSessionRole_SuspendedDetachesBaseline(t *testing.T) {
	const baseline = "arn:aws:iam::aws:policy/EksAgentBaseline"
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{TenantBaselinePolicyARN: baseline, Environment: "production"}
	p := attributedPlatform("acme", "protohype", []string{"alice@acme.com"}, nil)
	name := sessionRoleName(cfg.Environment, p)
	f.seedRole(name, "arn:aws:iam::123:role/"+name)
	f.seedAttachment(name, baseline)

	if _, err := r.ensureSessionRole(context.Background(), p, "arn:aws:iam::1:role/tenant", true, cfg); err != nil {
		t.Fatalf("ensureSessionRole (suspended): %v", err)
	}
	if got := f.attachmentsFor(name); len(got) != 0 {
		t.Errorf("baseline must be detached when suspended (kill-switch parity): got %v", got)
	}
}

func TestEnsureSessionRole_NilAttributionNoop(t *testing.T) {
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	p := newPlatform("acme", "protohype") // no attribution

	arn, err := r.ensureSessionRole(context.Background(), p, "arn:aws:iam::1:role/tenant", false, IAMConfig{Environment: "production"})
	if err != nil || arn != "" {
		t.Fatalf("expected no-op; got arn=%q err=%v", arn, err)
	}
	if len(f.createCalls) != 0 {
		t.Errorf("expected no create calls, got %d", len(f.createCalls))
	}
}

func TestDeleteSessionRole(t *testing.T) {
	f := newFakeIAM()
	r := &PlatformReconciler{IAM: f}
	cfg := IAMConfig{Environment: "production"}
	p := attributedPlatform("acme", "protohype", []string{"alice@acme.com"}, nil)
	name := sessionRoleName(cfg.Environment, p)
	f.seedRole(name, "arn:aws:iam::123:role/"+name)
	f.seedAttachment(name, "arn:aws:iam::aws:policy/EksAgentBaseline")

	if err := r.deleteSessionRole(context.Background(), p, cfg.Environment); err != nil {
		t.Fatalf("deleteSessionRole: %v", err)
	}
	if _, ok := f.roles[name]; ok {
		t.Errorf("session role should be deleted")
	}
	// Deleting a non-existent session role is a tolerated no-op.
	if err := r.deleteSessionRole(context.Background(), p, cfg.Environment); err != nil {
		t.Errorf("second delete should be a no-op: %v", err)
	}
}
