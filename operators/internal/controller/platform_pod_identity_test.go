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

	"github.com/aws/aws-sdk-go-v2/aws"
)

const (
	podIdentityTestRole  = "arn:aws:iam::123456789012:role/eks-agent-platform/tenants/c-analytics-eval"
	podIdentityTestOther = "arn:aws:iam::123456789012:role/someone-else"
)

// TestEnsurePodIdentityAssociation_ConvergedWritesNothing: an association that
// already hands the account the role's own credentials is kept as it is.
func TestEnsurePodIdentityAssociation_ConvergedWritesNothing(t *testing.T) {
	fe := newFakeEKS()
	fe.seedAssociation("ns", "sa", podIdentityTestRole, "")
	r := &PlatformReconciler{EKS: fe}
	if err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", "sa", podIdentityTestRole); err != nil {
		t.Fatal(err)
	}
	if len(fe.createCalls) != 0 || len(fe.deleteCalls) != 0 {
		t.Errorf("a converged binding was rewritten: CreatePodIdentityAssociation %d, DeletePodIdentityAssociation %d",
			len(fe.createCalls), len(fe.deleteCalls))
	}
}

// TestEnsurePodIdentityAssociation_ReplacesAnyOtherBinding: an association that
// hands the account any credentials but the role's own — another role, the
// role chaining to a target, or one EKS describes as nothing — is deleted and
// the binding created in its place. The account cannot hold two associations,
// so the delete comes first.
func TestEnsurePodIdentityAssociation_ReplacesAnyOtherBinding(t *testing.T) {
	cases := []struct {
		name         string
		role, target string
		undescribed  bool
	}{
		{name: "another role", role: podIdentityTestOther},
		{name: "a target role chained from it", role: podIdentityTestRole, target: podIdentityTestOther},
		{name: "an undescribed association", undescribed: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := newFakeEKS()
			id := fe.seedAssociation("ns", "sa", c.role, c.target)
			if c.undescribed {
				fe.bound[id] = nil
			}
			r := &PlatformReconciler{EKS: fe}
			if err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", "sa", podIdentityTestRole); err != nil {
				t.Fatal(err)
			}
			if len(fe.deleteCalls) != 1 || aws.ToString(fe.deleteCalls[0].AssociationId) != id {
				t.Errorf("DeletePodIdentityAssociation calls %+v, want the one for %s", fe.deleteCalls, id)
			}
			a := fe.bound[fe.associations["ns/sa"]]
			if !bindsExactly(a, podIdentityTestRole) {
				t.Errorf("ns/sa is bound to %+v, want %s and no target role", a, podIdentityTestRole)
			}
		})
	}
}

func TestEnsurePodIdentityAssociation_ConvergenceErrors(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name         string
		role, target string
		undescribed  bool
		inject       func(*fakeEKS)
		want         string
	}{
		{name: "describe", role: podIdentityTestRole, inject: func(fe *fakeEKS) { fe.describeReturnsErr = boom }, want: "eks DescribePodIdentityAssociation a-1"},
		{
			name: "replacing another role", role: podIdentityTestOther,
			inject: func(fe *fakeEKS) { fe.deleteReturnsErr = boom },
			want:   "eks DeletePodIdentityAssociation a-1 (bound to " + podIdentityTestOther + ", not " + podIdentityTestRole + ")",
		},
		{
			name: "replacing a chain", role: podIdentityTestRole, target: podIdentityTestOther,
			inject: func(fe *fakeEKS) { fe.deleteReturnsErr = boom },
			want:   "(bound to " + podIdentityTestRole + " chaining to " + podIdentityTestOther + ", not " + podIdentityTestRole + ")",
		},
		{
			name: "replacing an undescribed association", undescribed: true,
			inject: func(fe *fakeEKS) { fe.deleteReturnsErr = boom },
			want:   "(bound to an undescribed role, not " + podIdentityTestRole + ")",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := newFakeEKS()
			id := fe.seedAssociation("ns", "sa", c.role, c.target)
			if c.undescribed {
				fe.bound[id] = nil
			}
			c.inject(fe)
			r := &PlatformReconciler{EKS: fe}
			err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", "sa", podIdentityTestRole)
			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error wrapping boom that names %q", err, c.want)
			}
			if len(fe.createCalls) != 0 {
				t.Error("created a binding while another still held the account")
			}
		})
	}
}

// TestEnsureIamRole_RebindsTheTenantAssociation: status.podIdentity reports the
// tenant role as the account's binding, so an association naming another role
// is replaced before that is published.
func TestEnsureIamRole_RebindsTheTenantAssociation(t *testing.T) {
	f, fe := newFakeIAM(), newFakeEKS()
	platform := newPlatform("analytics", "acme")
	fe.seedAssociation(PlatformNamespace(platform), tenantSAName, podIdentityTestOther, "")
	r := &PlatformReconciler{IAM: f, EKS: fe}
	got, err := r.ensureIamRole(context.Background(), platform, IAMConfig{ClusterName: "production-cluster", Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if bound := fe.roleFor(PlatformNamespace(platform), tenantSAName); bound != got.RoleARN || got.PodIdentity.RoleArn != got.RoleARN {
		t.Errorf("tenant-runtime is bound to %q and reported as %q, want the tenant role %s", bound, got.PodIdentity.RoleArn, got.RoleARN)
	}
}
