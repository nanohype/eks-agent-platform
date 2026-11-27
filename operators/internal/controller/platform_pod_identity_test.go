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

	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

const (
	podIdentityTestRole  = "arn:aws:iam::123456789012:role/eks-agent-platform/tenants/c-analytics-eval"
	podIdentityTestOther = "arn:aws:iam::123456789012:role/someone-else"
)

// TestEnsurePodIdentityAssociation_ConvergedWritesNothing: an association that
// already hands the account the role's own credentials is kept as it is.
func TestEnsurePodIdentityAssociation_ConvergedWritesNothing(t *testing.T) {
	fe := awsfake.NewEKS()
	fe.SeedAssociation("ns", "sa", podIdentityTestRole, "")
	r := &PlatformReconciler{EKS: fe}
	if err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", "sa", podIdentityTestRole); err != nil {
		t.Fatal(err)
	}
	if len(fe.CreateCalls) != 0 || len(fe.DeleteCalls) != 0 {
		t.Errorf("a converged binding was rewritten: CreatePodIdentityAssociation %d, DeletePodIdentityAssociation %d",
			len(fe.CreateCalls), len(fe.DeleteCalls))
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
			fe := awsfake.NewEKS()
			id := fe.SeedAssociation("ns", "sa", c.role, c.target)
			if c.undescribed {
				fe.Bound[id] = nil
			}
			r := &PlatformReconciler{EKS: fe}
			if err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", "sa", podIdentityTestRole); err != nil {
				t.Fatal(err)
			}
			if len(fe.DeleteCalls) != 1 || aws.ToString(fe.DeleteCalls[0].AssociationId) != id {
				t.Errorf("DeletePodIdentityAssociation calls %+v, want the one for %s", fe.DeleteCalls, id)
			}
			a := fe.Bound[fe.Associations["ns/sa"]]
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
		inject       func(*awsfake.EKS)
		want         string
	}{
		{name: "describe", role: podIdentityTestRole, inject: func(fe *awsfake.EKS) { fe.DescribeReturnsErr = boom }, want: "eks DescribePodIdentityAssociation a-1"},
		{
			name: "replacing another role", role: podIdentityTestOther,
			inject: func(fe *awsfake.EKS) { fe.DeleteReturnsErr = boom },
			want:   "eks DeletePodIdentityAssociation a-1 (bound to " + podIdentityTestOther + ", not " + podIdentityTestRole + ")",
		},
		{
			name: "replacing a chain", role: podIdentityTestRole, target: podIdentityTestOther,
			inject: func(fe *awsfake.EKS) { fe.DeleteReturnsErr = boom },
			want:   "(bound to " + podIdentityTestRole + " chaining to " + podIdentityTestOther + ", not " + podIdentityTestRole + ")",
		},
		{
			name: "replacing an undescribed association", undescribed: true,
			inject: func(fe *awsfake.EKS) { fe.DeleteReturnsErr = boom },
			want:   "(bound to an undescribed role, not " + podIdentityTestRole + ")",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := awsfake.NewEKS()
			id := fe.SeedAssociation("ns", "sa", c.role, c.target)
			if c.undescribed {
				fe.Bound[id] = nil
			}
			c.inject(fe)
			r := &PlatformReconciler{EKS: fe}
			err := r.ensurePodIdentityAssociation(context.Background(), IAMConfig{ClusterName: "c"}, "ns", "sa", podIdentityTestRole)
			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error wrapping boom that names %q", err, c.want)
			}
			if len(fe.CreateCalls) != 0 {
				t.Error("created a binding while another still held the account")
			}
		})
	}
}

// TestEnsureIamRole_RebindsTheTenantAssociation: status.podIdentity reports the
// tenant role as the account's binding, so an association naming another role
// is replaced before that is published.
func TestEnsureIamRole_RebindsTheTenantAssociation(t *testing.T) {
	f, fe := awsfake.NewIAM(), awsfake.NewEKS()
	platform := newPlatform("analytics", "acme")
	fe.SeedAssociation(PlatformNamespace(platform), tenantSAName, podIdentityTestOther, "")
	r := &PlatformReconciler{IAM: f, EKS: fe}
	got, err := r.ensureIamRole(context.Background(), platform, IAMConfig{ClusterName: "production-cluster", Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if bound := fe.RoleFor(PlatformNamespace(platform), tenantSAName); bound != got.RoleARN || got.PodIdentity.RoleArn != got.RoleARN {
		t.Errorf("tenant-runtime is bound to %q and reported as %q, want the tenant role %s", bound, got.PodIdentity.RoleArn, got.RoleARN)
	}
}

// TestEnsureIamRole_GatewayBindingFailureIsReported: the gateway's association
// is part of the Platform's identity, so failing to bind it fails the pass on
// both the fresh-role and existing-role paths — and the tenant binding that did
// land is still reported, while no gateway binding is. A published
// gatewayPodIdentity is what releases the ModelGateway to render, so reporting
// one that was never made would admit an Envoy pod with no credentials.
func TestEnsureIamRole_GatewayBindingFailureIsReported(t *testing.T) {
	boom := errors.New("boom")
	platform := newPlatform("analytics", "acme")
	gatewaySA := gatewayServiceAccountName(platform)
	cfg := IAMConfig{ClusterName: "production-cluster", Environment: "production"}

	check := func(t *testing.T, got iamReconcileResult, err error) {
		t.Helper()
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "gateway ServiceAccount") {
			t.Fatalf("got %v, want the gateway binding's error, naming it", err)
		}
		if got.PodIdentity == nil || got.PodIdentity.ServiceAccount != tenantSAName {
			t.Errorf("the tenant binding landed and must still be reported, got %+v", got.PodIdentity)
		}
		if got.GatewayPodIdentity != nil {
			t.Errorf("a gateway binding that was never made was reported: %+v", got.GatewayPodIdentity)
		}
	}

	t.Run("fresh role", func(t *testing.T) {
		fe := awsfake.NewEKS()
		fe.CreateReturnsErr, fe.FailOnlyFor = boom, gatewaySA
		r := &PlatformReconciler{IAM: awsfake.NewIAM(), EKS: fe}
		got, err := r.ensureIamRole(context.Background(), platform, cfg)
		check(t, got, err)
	})

	t.Run("existing role", func(t *testing.T) {
		fe := awsfake.NewEKS()
		r := &PlatformReconciler{IAM: awsfake.NewIAM(), EKS: fe}
		if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
			t.Fatalf("first pass: %v", err)
		}
		delete(fe.Associations, PlatformNamespace(platform)+"/"+gatewaySA)
		fe.CreateReturnsErr, fe.FailOnlyFor = boom, gatewaySA
		got, err := r.ensureIamRole(context.Background(), platform, cfg)
		check(t, got, err)
	})
}

// TestDeleteIamRole_GatewayAssociationFailureHoldsTheFinalizer: the role is
// deleted only after both associations are gone, so a failure removing the
// gateway's stops the teardown before the role rather than orphaning an
// association that names a deleted role.
func TestDeleteIamRole_GatewayAssociationFailureHoldsTheFinalizer(t *testing.T) {
	boom := errors.New("boom")
	platform := newPlatform("analytics", "acme")
	f, fe := awsfake.NewIAM(), awsfake.NewEKS()
	r := &PlatformReconciler{IAM: f, EKS: fe}
	cfg := IAMConfig{ClusterName: "production-cluster", Environment: "production"}
	if _, err := r.ensureIamRole(context.Background(), platform, cfg); err != nil {
		t.Fatal(err)
	}
	fe.ListReturnsErr, fe.FailOnlyFor = boom, gatewayServiceAccountName(platform)
	if err := r.deleteIamRole(context.Background(), platform, cfg); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the gateway association's error", err)
	}
	if fe.RoleFor(PlatformNamespace(platform), gatewayServiceAccountName(platform)) == "" {
		t.Error("the gateway association is gone although its removal failed")
	}
}
