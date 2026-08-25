/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The tenant boundary is two policies, and this pins the second one.
//
// A NetworkPolicy that omits Ingress from PolicyTypes does not deny ingress —
// it says nothing about ingress, which reads identically to a correct policy
// and differs in exactly the way that matters. AgentFleet, AgentSandbox and
// SandboxPool pods each carry their own deny-all, so the pods this covers that
// nothing else does are the tenant's own application pods.
func TestTenantIngressPolicyDeniesByDefault(t *testing.T) {
	s := fleetScheme(t)
	p := readyPlatformIn()
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(p).Build()
	r := &PlatformReconciler{Client: cl, Scheme: s}

	if err := r.ensureTenantIngressPolicy(context.Background(), p); err != nil {
		t.Fatalf("ensureTenantIngressPolicy: %v", err)
	}

	var np networkingv1.NetworkPolicy
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: "tenant-ingress"}
	if err := cl.Get(context.Background(), key, &np); err != nil {
		t.Fatalf("tenant-ingress not created: %v", err)
	}

	// Naming the Ingress policy type is what makes this a deny rather than a
	// silence. Without it every rule below is decoration.
	var hasIngressType bool
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeIngress {
			hasIngressType = true
		}
	}
	if !hasIngressType {
		t.Error("tenant-ingress omits PolicyTypes: Ingress, so it restricts no ingress at all")
	}

	// Namespace-wide: an empty PodSelector is every pod, which is the point —
	// the tenant's own application pods are the ones nothing else covers.
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("tenant-ingress selects %v, not every pod in the namespace", np.Spec.PodSelector)
	}

	// Two allows, and no more. Each one is a path that fails silently when
	// absent, which is why they are named rather than left to the tenant chart.
	var sameNamespaceGateway, fromCollector bool
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			switch {
			case peer.NamespaceSelector == nil && peer.PodSelector != nil:
				// No NamespaceSelector means this namespace. Scoped to the
				// gateway port: an application pod may reach its own Envoy, and
				// the egress rule that lets it send is useless without this.
				for _, port := range rule.Ports {
					if port.Port != nil && port.Port.IntValue() == 8080 {
						sameNamespaceGateway = true
					}
				}
			case peer.NamespaceSelector != nil:
				if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == collectorNamespace {
					fromCollector = true
				}
			}
		}
	}
	if !sameNamespaceGateway {
		t.Errorf("tenant-ingress does not allow same-namespace ingress to the gateway on 8080; "+
			"model calls would time out against a Gateway reporting healthy (rules: %+v)", np.Spec.Ingress)
	}
	if !fromCollector {
		t.Errorf("tenant-ingress does not allow ingress from %q; a metrics scrape is ingress, and "+
			"blocking it reads as a quiet workload rather than a blocked one", collectorNamespace)
	}
}
