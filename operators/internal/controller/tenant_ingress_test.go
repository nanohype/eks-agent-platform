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

// A read-only root filesystem is only usable alongside somewhere to write, so
// the two are asserted together. Pod Security's "restricted" profile does not
// require readOnlyRootFilesystem, which is why the namespace label alone left
// the pods executing untrusted tool calls with a writable root while the
// operator's own pod had one — the hardening was inverted relative to the trust
// each deserves.
func TestSandboxContainerSecurityContextIsReadOnlyAndHasSomewhereToWrite(t *testing.T) {
	sc := sandboxContainerSecurityContext()
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("sandbox containers run untrusted agent code with a writable root filesystem")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("sandbox containers allow privilege escalation")
	}

	// The tenant's own long-running image keeps a writable root deliberately:
	// this repo has no contract with it, and a hardening measure that stops an
	// arbitrary image from starting is one an operator disables wholesale.
	if fleet := restrictedContainerSecurityContext(); fleet.ReadOnlyRootFilesystem != nil {
		t.Error("the fleet profile now forces a read-only root on the tenant's own image; that is a different decision and needs its own opt-out")
	}

	// The paths this operator may name for an ARBITRARY image are the two it
	// already owns. Everything else is a fact about the image, and deriving one
	// image's layout and applying it to another is how a read-only root becomes
	// a workload that cannot start — the first version of this helper mounted
	// /home/worker for the tenant-supplied AgentSandbox image too, where it is a
	// guess.
	base := sandboxWritableMounts()
	got := map[string]string{}
	for _, m := range base {
		got[m.MountPath] = m.Name
	}
	for _, want := range []string{"/workspace", "/tmp"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no writable mount at %s; with a read-only root a write there fails inside the tool call", want)
		}
	}
	if _, ok := got["/home/worker"]; ok {
		t.Error("/home/worker is mounted for every sandbox; it is the sandbox-worker image's HOME and a guess for any tenant image")
	}

	// A caller that KNOWS its image adds to that set.
	withHome := sandboxWritableMounts("/home/worker")
	if len(withHome) != len(base)+1 {
		t.Errorf("declaring an extra path yielded %d mounts, want %d", len(withHome), len(base)+1)
	}

	// Volumes and mounts have to agree, or the pod does not start.
	vols := sandboxWritableVolumes("/home/worker")
	if len(vols) != len(withHome) {
		t.Fatalf("%d mount(s) against %d volume(s)", len(withHome), len(vols))
	}
	names := map[string]bool{}
	for _, v := range vols {
		if v.EmptyDir == nil {
			t.Errorf("writable volume %q is not an emptyDir; a sandbox that outlives its session is not single-use", v.Name)
		}
		names[v.Name] = true
	}
	for _, m := range withHome {
		if !names[m.Name] {
			t.Errorf("mount %q names no volume", m.Name)
		}
	}

	// A duplicate declaration must not produce two volumes with one name.
	if dup := sandboxWritableVolumes("/tmp", "/tmp"); len(dup) != len(base) {
		t.Errorf("a duplicate path produced %d volumes, want %d", len(dup), len(base))
	}

	// A tenant-declared path has to render an RFC-1123 volume name.
	for _, v := range sandboxWritableVolumes("/var/cache/tool_x") {
		for _, r := range v.Name {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				t.Errorf("volume name %q carries %q, which the API server rejects", v.Name, r)
			}
		}
	}
}
