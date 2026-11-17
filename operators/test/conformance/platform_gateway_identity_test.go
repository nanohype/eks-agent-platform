/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

// TestPlatformReconciler_PublishesTheGatewayIdentityTheModelGatewayWaitsOn
// drives the whole chain through the API server: PlatformReconciler.Reconcile
// binds the gateway's ServiceAccount and publishes it as
// status.gatewayPodIdentity, and a ModelGateway reconciled against the Platform
// as stored gets past the wait for that binding.
//
// The ModelGateway holds every render while status.podIdentity is set and
// status.gatewayPodIdentity is not, so a Platform reconcile that bound the
// association and dropped the status field would park every gateway on an EKS
// cluster in Pending for good. Tests that set the Platform's status by hand or
// call the IAM step directly cannot see that; this one reads what Reconcile
// wrote.
func TestPlatformReconciler_PublishesTheGatewayIdentityTheModelGatewayWaitsOn(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	eksFake := newMemEKS()
	r := newScopingReconciler(newMemIAM())
	r.EKS = eksFake
	p := scopingPlatform(ctx, t, platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}})
	reconcileIAM(ctx, t, r, p)

	got := getPlatform(ctx, t, p.Namespace, p.Name)
	tenantNS := controller.PlatformNamespace(p)
	gatewaySA := p.Name + "-gateway"
	if got.Status.Phase != phaseReady {
		t.Fatalf("Platform phase: got %q want Ready", got.Status.Phase)
	}
	if got.Status.IamRoleArn == "" {
		t.Fatal("status.iamRoleArn empty after an IAM-wired reconcile")
	}
	if pi := got.Status.PodIdentity; pi == nil || pi.ServiceAccount != "tenant-runtime" {
		t.Fatalf("status.podIdentity: got %+v want the tenant-runtime binding", pi)
	}
	gw := got.Status.GatewayPodIdentity
	if gw == nil {
		t.Fatal("status.gatewayPodIdentity not published by Reconcile")
	}
	want := platformv1alpha1.PodIdentityStatus{
		ClusterName:    scopingEnv,
		Namespace:      tenantNS,
		ServiceAccount: gatewaySA,
		RoleArn:        got.Status.IamRoleArn,
	}
	if *gw != want {
		t.Errorf("status.gatewayPodIdentity: got %+v want %+v", *gw, want)
	}
	if role, ok := eksFake.roleFor(tenantNS, gatewaySA); !ok || role != got.Status.IamRoleArn {
		t.Errorf("EKS association for %s/%s: got (%q, %v) want the tenant role %q", tenantNS, gatewaySA, role, ok, got.Status.IamRoleArn)
	}

	mg := &agentsv1alpha1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "g"), Namespace: testNs},
		Spec: agentsv1alpha1.ModelGatewaySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: p.Name},
			Routes: []agentsv1alpha1.ModelRouteSpec{
				{Name: "primary", ModelFamily: "anthropic", ModelID: "us.anthropic.claude-3-5-sonnet-20241022-v2:0"},
			},
		},
	}
	mustCreate(ctx, t, mg)
	reconcileGateway(ctx, t, mg)

	var stored agentsv1alpha1.ModelGateway
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mg.Name, Namespace: mg.Namespace}, &stored); err != nil {
		t.Fatalf("get ModelGateway: %v", err)
	}
	// envtest installs no Gateway-API / Envoy AI Gateway CRDs, so the furthest
	// a ModelGateway gets here is Pending on those. What it must not be waiting
	// on is the identity binding Reconcile just published.
	rr := apimeta.FindStatusCondition(stored.Status.Conditions, "RoutesReconciled")
	if rr == nil {
		t.Fatal("no RoutesReconciled condition written")
	}
	if strings.Contains(rr.Message, "gatewayPodIdentity") {
		t.Errorf("the ModelGateway still waits on the binding Reconcile published: %s", rr.Message)
	}
	if !strings.Contains(rr.Message, "CRDs") {
		t.Errorf("RoutesReconciled: got %q, want the wait on the data-plane CRDs envtest lacks", rr.Message)
	}
}
