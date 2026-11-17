/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// TestModelGatewayReadyFollowsTheDataPlane holds Ready to what Envoy Gateway
// reports about the rendered Gateway, not to the reconciler having applied it.
//
// The failure this guards against was observed live: Envoy Gateway 1.10.0 in
// GatewayNamespace mode refused to adopt the proxy ServiceAccount the EnvoyProxy
// named, rendered no Deployment and no Service, and the Gateway sat at
// Programmed=False/AddressNotAssigned — while the ModelGateway reported Ready
// and published a base URL with no pod behind it. Every applied object was
// fine, which is why the apply cannot be the readiness signal.
func TestModelGatewayReadyFollowsTheDataPlane(t *testing.T) {
	cases := []struct {
		name string
		// program writes the Gateway's status the way Envoy Gateway would; nil
		// leaves it with none, as before the controller has looked at it.
		program    func(t *testing.T, gw *unstructured.Unstructured, write func(status, reason string, observed int64))
		wantPhase  string
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string
	}{
		{
			name:       "no status yet",
			wantPhase:  phaseProvisioning,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonAwaitingGatewayStatus,
			wantInMsg:  gatewayClassName,
		},
		{
			// The live shape: no Service, so no address, so not Programmed.
			name: "Envoy Gateway rendered no data plane",
			program: func(_ *testing.T, gw *unstructured.Unstructured, write func(string, string, int64)) {
				write("False", "AddressNotAssigned", gw.GetGeneration())
			},
			wantPhase:  phaseProvisioning,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonGatewayNotProgrammed,
			wantInMsg:  "AddressNotAssigned",
		},
		{
			// A True written for an older spec says nothing about this one.
			name: "programmed for an earlier generation",
			program: func(t *testing.T, gw *unstructured.Unstructured, write func(string, string, int64)) {
				if gw.GetGeneration() < 1 {
					t.Fatalf("the rendered Gateway carries generation %d; this case needs one to be stale against", gw.GetGeneration())
				}
				write("True", "Programmed", gw.GetGeneration()-1)
			},
			wantPhase:  phaseProvisioning,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonAwaitingGatewayStatus,
		},
		{
			name: "programmed",
			program: func(_ *testing.T, gw *unstructured.Unstructured, write func(string, string, int64)) {
				write("True", "Programmed", gw.GetGeneration())
			},
			wantPhase:  phaseReady,
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonDataPlaneProgrammed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mgwScheme(t)
			mg := twoRouteGateway()
			cl := fake.NewClientBuilder().WithScheme(s).
				WithObjects(readyTestPlatform(), mg).WithStatusSubresource(mg).Build()
			r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
			ctx := context.Background()

			if _, err := r.reconcileSelf(ctx, mg); err != nil {
				t.Fatalf("first reconcileSelf: %v", err)
			}
			gw := getRendered(t, cl, gatewayAPIGV, "Gateway", ctrlTestPlatform+"-gateway")
			if gw.GetGeneration() == 0 {
				// The fake client leaves generation unset on an unstructured
				// kind it has no type for; give it the API server's starting value.
				gw.SetGeneration(1)
			}
			if tc.program != nil {
				tc.program(t, gw, func(status, reason string, observed int64) {
					setGatewayCondition(t, cl, gw, status, reason, observed)
				})
			} else if err := cl.Update(ctx, gw); err != nil {
				t.Fatal(err)
			}

			res, err := r.reconcileSelf(ctx, mg)
			if err != nil {
				t.Fatalf("reconcileSelf: %v", err)
			}
			if res.phase != tc.wantPhase {
				t.Fatalf("phase: got %q want %q (data plane %+v)", res.phase, tc.wantPhase, res.dataPlane)
			}
			if tc.wantPhase != phaseReady && (res.endpoint != "" || len(res.routes) != 0) {
				t.Errorf("a gateway with no serving data plane published endpoint %q and routes %v", res.endpoint, res.routes)
			}
			if tc.wantPhase == phaseReady && (res.endpoint == "" || len(res.routes) != len(mg.Spec.Routes)) {
				t.Errorf("a programmed gateway must publish its contract, got endpoint %q routes %v", res.endpoint, res.routes)
			}

			if err := r.modelGatewayApplyStatus(ctx, mg, res); err != nil {
				t.Fatalf("modelGatewayApplyStatus: %v", err)
			}
			if mg.Status.Phase != tc.wantPhase {
				t.Errorf("status.phase: got %q want %q", mg.Status.Phase, tc.wantPhase)
			}
			dp := findCondition(mg.Status.Conditions, dataPlaneConditionType)
			if dp == nil {
				t.Fatalf("no %s condition written", dataPlaneConditionType)
			}
			if dp.Status != tc.wantStatus || dp.Reason != tc.wantReason {
				t.Errorf("%s: got %s/%s want %s/%s (%s)", dataPlaneConditionType, dp.Status, dp.Reason, tc.wantStatus, tc.wantReason, dp.Message)
			}
			if tc.wantInMsg != "" && !strings.Contains(dp.Message, tc.wantInMsg) {
				t.Errorf("%s message %q does not name %q", dataPlaneConditionType, dp.Message, tc.wantInMsg)
			}
			// The routes were applied on every one of these passes, so the
			// step that names them is done even where the data plane is not.
			if rr := findCondition(mg.Status.Conditions, "RoutesReconciled"); rr == nil || rr.Status != metav1.ConditionTrue {
				t.Errorf("RoutesReconciled: got %+v want True", rr)
			}
			if g := findCondition(mg.Status.Conditions, "ImportedRouteGuardrailUnenforced"); g == nil || g.Status != metav1.ConditionFalse {
				t.Errorf("the routes were walked, so the guardrail condition is an answer, got %+v", g)
			}
		})
	}
}

// TestModelGatewayWaitsForTheGatewayIdentity: on a cluster that binds identities
// with Pod Identity, no gateway is rendered until the Platform has bound the
// gateway's ServiceAccount. The Pod Identity webhook injects credentials at pod
// admission only, so an Envoy pod admitted before the association would run
// without them until restarted — and Envoy Gateway would still report it
// Programmed, since the pod is available.
func TestModelGatewayWaitsForTheGatewayIdentity(t *testing.T) {
	s := mgwScheme(t)
	ctx := context.Background()
	p := readyTestPlatform()
	p.Status.PodIdentity = &platformv1alpha1.PodIdentityStatus{
		ClusterName: "c", Namespace: PlatformNamespace(p), ServiceAccount: tenantSAName, RoleArn: "arn:aws:iam::123456789012:role/t",
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}

	res, err := r.reconcileSelf(ctx, twoRouteGateway())
	if err != nil {
		t.Fatalf("reconcileSelf: %v", err)
	}
	if res.phase != phasePending || !strings.Contains(res.waitingOn, "gatewayPodIdentity") {
		t.Fatalf("an unbound gateway identity: got phase %q waiting on %q, want Pending naming status.gatewayPodIdentity", res.phase, res.waitingOn)
	}
	for _, kind := range []string{"EnvoyProxy", "Gateway"} {
		gv := envoyGatewayGV
		if kind == "Gateway" {
			gv = gatewayAPIGV
		}
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(gv.WithKind(kind))
		key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: ctrlTestPlatform + "-gateway"}
		if err := cl.Get(ctx, key, o); err == nil {
			t.Errorf("%s rendered before the gateway's ServiceAccount was bound", kind)
		}
	}

	p.Status.GatewayPodIdentity = &platformv1alpha1.PodIdentityStatus{
		ClusterName: "c", Namespace: PlatformNamespace(p), ServiceAccount: gatewayServiceAccountName(p), RoleArn: p.Status.PodIdentity.RoleArn,
	}
	if err := cl.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	res, err = r.reconcileSelf(ctx, twoRouteGateway())
	if err != nil {
		t.Fatalf("reconcileSelf (bound): %v", err)
	}
	if res.phase != phaseProvisioning {
		t.Errorf("a bound gateway identity: got phase %q want Provisioning (rendered, not yet programmed)", res.phase)
	}
}

// TestDataPlaneConditionIsUnknownWhenNothingWasRendered: a pass that returned
// before the Gateway was applied has no report to relay. False there would read
// as Envoy Gateway refusing a Gateway it was never handed.
func TestDataPlaneConditionIsUnknownWhenNothingWasRendered(t *testing.T) {
	mg := twoRouteGateway()
	for _, res := range []gatewayReconcileResult{
		{phase: phasePending, waitingOn: gatewayPrerequisitesMessage},
		{phase: phasePending, dataPlane: dataPlaneState{status: metav1.ConditionFalse, reason: reasonGatewayNotProgrammed}},
	} {
		c := dataPlaneCondition(mg, res)
		if c.Status != metav1.ConditionUnknown || c.Reason != reasonDataPlaneNotRendered {
			t.Errorf("pending pass %+v: got %s/%s want Unknown/%s", res, c.Status, c.Reason, reasonDataPlaneNotRendered)
		}
	}
}

// TestModelGatewayReconcilePollsAProvisioningGateway: nothing watches the
// Gateway, so a Provisioning pass must schedule its own next look or the
// ModelGateway stays Provisioning after the data plane comes up.
func TestModelGatewayReconcilePollsAProvisioningGateway(t *testing.T) {
	s := mgwScheme(t)
	mg := twoRouteGateway()
	mg.Finalizers = []string{modelGatewayFinalizer}
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(readyTestPlatform(), mg).WithStatusSubresource(mg).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}

	got, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mg.Namespace, Name: mg.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.RequeueAfter <= 0 {
		t.Errorf("a Provisioning pass returned %+v; nothing else would re-drive it", got)
	}
	var stored agentsv1alpha1.ModelGateway
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: mg.Namespace, Name: mg.Name}, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != phaseProvisioning || stored.Status.Endpoint != "" {
		t.Errorf("stored status: phase %q endpoint %q, want Provisioning with no endpoint", stored.Status.Phase, stored.Status.Endpoint)
	}
}

// TestModelGatewayReadyIsReChecked: Ready is a reading of the Gateway's
// Programmed condition, and nothing watches the Gateway. A Ready pass must
// schedule its own next look, and that look must take the ModelGateway back to
// Provisioning — endpoint withdrawn — once the data plane stops serving.
func TestModelGatewayReadyIsReChecked(t *testing.T) {
	s := mgwScheme(t)
	ctx := context.Background()
	mg := twoRouteGateway()
	mg.Finalizers = []string{modelGatewayFinalizer}
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(readyTestPlatform(), mg).WithStatusSubresource(mg).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mg.Namespace, Name: mg.Name}}
	stored := func() agentsv1alpha1.ModelGateway {
		t.Helper()
		var got agentsv1alpha1.ModelGateway
		if err := cl.Get(ctx, req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (render): %v", err)
	}
	setGatewayProgrammed(t, cl, "True", "Programmed")
	got, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile (programmed): %v", err)
	}
	if st := stored(); st.Status.Phase != phaseReady || st.Status.Endpoint == "" {
		t.Fatalf("programmed gateway: phase %q endpoint %q, want Ready with an endpoint", st.Status.Phase, st.Status.Endpoint)
	}
	if got.RequeueAfter <= 0 {
		t.Fatalf("a Ready pass returned %+v; nothing else would notice the data plane going away", got)
	}

	// Envoy Gateway withdraws Programmed — the Envoy Deployment lost its
	// available replica.
	setGatewayProgrammed(t, cl, "False", "NoResources")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (unprogrammed): %v", err)
	}
	st := stored()
	if st.Status.Phase != phaseProvisioning || st.Status.Endpoint != "" || len(st.Status.Routes) != 0 {
		t.Errorf("a gateway that stopped serving: phase %q endpoint %q routes %v, want Provisioning with nothing published",
			st.Status.Phase, st.Status.Endpoint, st.Status.Routes)
	}
	if dp := findCondition(st.Status.Conditions, dataPlaneConditionType); dp == nil || dp.Reason != reasonGatewayNotProgrammed || !strings.Contains(dp.Message, "NoResources") {
		t.Errorf("%s: got %+v want False/%s relaying NoResources", dataPlaneConditionType, dp, reasonGatewayNotProgrammed)
	}
}

// TestModelGatewayGatewayKindGoneOnReadIsPending: a Gateway CRD removed between
// the apply and the status read is the same bootstrap ordering a missing CRD
// is on the apply, and reads as Pending rather than as a reconcile error.
func TestModelGatewayGatewayKindGoneOnReadIsPending(t *testing.T) {
	s := mgwScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(readyTestPlatform()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				// Only the read after the apply: the apply's own lookup finds
				// nothing and creates the Gateway, so the kind was there then.
				if u, ok := obj.(*unstructured.Unstructured); ok && err == nil && u.GetKind() == "Gateway" && u.GetAPIVersion() == gatewayAPIGV.String() {
					return &apimeta.NoKindMatchError{GroupKind: gatewayAPIGV.WithKind("Gateway").GroupKind()}
				}
				return err
			},
		}).Build()
	r := &ModelGatewayReconciler{Client: cl, Scheme: s, Region: "us-west-2"}
	res, err := r.reconcileSelf(context.Background(), twoRouteGateway())
	if err != nil {
		t.Fatalf("reconcileSelf: %v", err)
	}
	if res.phase != phasePending || res.waitingOn != gatewayPrerequisitesMessage {
		t.Errorf("got phase %q waiting on %q, want Pending on the prerequisites", res.phase, res.waitingOn)
	}
}

func findCondition(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}
