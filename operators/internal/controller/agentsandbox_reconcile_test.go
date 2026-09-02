/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

// TestSessionPodIsConfinedToTheSandboxPool pins the placement and credential
// settings on the pod an AgentSandbox session runs in.
//
// This pod runs untrusted code — that is what a sandbox is for — and four
// fields keep it away from everything else. None were asserted: there was no
// unit test for this reconciler at all, and the conformance suite reads only
// RuntimeClassName.
//
// Every one fails silently, because the pod runs either way:
//
//	nodeSelector      without it the session schedules onto a general worker
//	                  node beside other tenants' agents instead of the dedicated
//	                  sandbox pool. The pod is Running and the isolation the
//	                  pool exists to provide is simply not there
//	tolerations       without them it cannot schedule onto that pool at all, so
//	                  the selector above strands it Pending
//	automount token   true mounts the tenant ServiceAccount token into the
//	                  sandbox, handing untrusted code the tenant's Pod Identity
//	                  credentials — the opposite of the boundary
//	restartPolicy     anything but Never turns a one-shot session into a pod
//	                  that restarts its workload after the session ends
//
// The selector and the toleration both derive from sandboxNodeLabel, so they
// cannot drift from each other by construction; what they can do is go missing
// from the pod, which is what this reads.
func TestSessionPodIsConfinedToTheSandboxPool(t *testing.T) {
	ctx := context.Background()
	p := newPlatform(ctrlTestPlatform, "team")
	box := &agentsv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: ctrlTestNS},
		Spec: agentsv1alpha1.AgentSandboxSpec{
			Image: "ghcr.io/acme/sandbox:v1",
		},
	}

	s := sandboxPoolScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentSandboxReconciler{Client: cl, Scheme: s}

	if err := r.ensureSessionPod(ctx, cl, box, p); err != nil {
		t.Fatalf("ensureSessionPod: %v", err)
	}

	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: PlatformNamespace(p), Name: agentSandboxResourceName(box)}
	if err := cl.Get(ctx, key, pod); err != nil {
		t.Fatalf("session pod not created: %v", err)
	}

	if got := pod.Spec.NodeSelector[sandboxNodeLabel]; got != "true" {
		t.Errorf("nodeSelector[%s] = %q, want \"true\" — without it an untrusted session schedules onto "+
			"a general worker node beside other tenants' agents, Running and unisolated",
			sandboxNodeLabel, got)
	}

	var tolerated bool
	for _, tol := range pod.Spec.Tolerations {
		if tol.Key == sandboxNodeLabel && tol.Effect == corev1.TaintEffectNoSchedule {
			tolerated = true
		}
	}
	if !tolerated {
		t.Errorf("pod does not tolerate the %s NoSchedule taint — with the nodeSelector above it would "+
			"never schedule at all", sandboxNodeLabel)
	}

	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken is not false — that mounts the tenant ServiceAccount token " +
			"into a pod running untrusted code, handing it the tenant's Pod Identity credentials")
	}

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never — a session is one-shot, and anything else restarts "+
			"the workload after it ends", pod.Spec.RestartPolicy)
	}

	// The hardened security contexts are shared with the pool's workers; read
	// here because this pod is the one running untrusted code.
	if pod.Spec.SecurityContext == nil {
		t.Error("pod has no securityContext — the restricted PSS profile is what keeps the session " +
			"from running as root")
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("want one container, got %d", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[0].SecurityContext == nil {
		t.Error("session container has no securityContext")
	}
}

// sandboxActiveDeadline resolves the spec field into the form a PodSpec takes.
// This covers that resolution and nothing further: the value it returns reaching
// a pod, and the coupling to the TTL that makes the ceiling load-bearing, are
// asserted against rendered artifacts in lifetime_bounds_test.go.
func TestSandboxActiveDeadline(t *testing.T) {
	secs := func(v int32) *int32 { return &v }

	cases := []struct {
		name string
		in   *int32
		want *int64
	}{
		{
			// Kubernetes rejects activeDeadlineSeconds: 0, so "disabled" has to
			// be an absent field rather than a zero one.
			name: "explicit zero disables rather than expiring instantly",
			in:   secs(0),
			want: nil,
		},
		{
			name: "unset leaves the field absent",
			in:   nil,
			want: nil,
		},
		{
			name: "a positive ceiling is carried through unchanged",
			in:   secs(14400),
			want: func() *int64 { v := int64(14400); return &v }(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			box := &agentsv1alpha1.AgentSandbox{}
			box.Spec.ActiveDeadlineSeconds = tc.in
			got := sandboxActiveDeadline(box)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %d, want the field absent", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got the field absent, want %d — a session with no ceiling holds its node slot and its tenant credentials until someone notices", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("got %d want %d", *got, *tc.want)
			}
		})
	}
}
