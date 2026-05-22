/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

func newAgentSandboxReconciler() *controller.AgentSandboxReconciler {
	return &controller.AgentSandboxReconciler{
		Client:      k8sClient,
		Scheme:      scheme,
		Concurrency: 1,
	}
}

func reconcileAgentSandbox(ctx context.Context, t *testing.T, box *agentsv1alpha1.AgentSandbox) {
	t.Helper()
	r := newAgentSandboxReconciler()
	// First call adds the finalizer + 100ms requeue; subsequent calls
	// drive the real reconcile.
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: box.Name, Namespace: box.Namespace}})
		if err != nil {
			t.Fatalf("agentsandbox reconcile attempt %d: %v", i+1, err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func TestAgentSandboxReconciler_PendingWhenPlatformMissing(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	box := &agentsv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "box"), Namespace: testNs},
		Spec: agentsv1alpha1.AgentSandboxSpec{
			PlatformRef: agentsv1alpha1.LocalRef{Name: "no-such-platform"},
			Image:       "ghcr.io/nanohype/fab:test",
		},
	}
	mustCreate(ctx, t, box)
	reconcileAgentSandbox(ctx, t, box)

	var got agentsv1alpha1.AgentSandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: box.Name, Namespace: box.Namespace}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != phasePending {
		t.Errorf("status.phase: got %q want phasePending (PlatformRef dangles)", got.Status.Phase)
	}
}

func TestAgentSandboxReconciler_SessionPodWhenPlatformReady(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)
	p := readySandboxPlatform(ctx, t)

	runtimeClass := "gvisor"
	// envtest enforces RuntimeClass existence — create the one the pod
	// references.
	rc := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: runtimeClass},
		Handler:    runtimeClass,
	}
	if err := k8sClient.Create(ctx, rc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create RuntimeClass: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rc) })

	box := &agentsv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "box"), Namespace: testNs},
		Spec: agentsv1alpha1.AgentSandboxSpec{
			PlatformRef:      agentsv1alpha1.LocalRef{Name: p.Name},
			Image:            "ghcr.io/nanohype/fab:test",
			Command:          []string{"node", "dist/bin/fab.js", "role-session"},
			RuntimeClassName: &runtimeClass,
		},
	}
	mustCreate(ctx, t, box)
	reconcileAgentSandbox(ctx, t, box)

	// The hardened session pod must have landed in the tenant namespace.
	var pod corev1.Pod
	key := types.NamespacedName{Namespace: p.Status.Namespace, Name: "session-" + box.Name}
	if err := k8sClient.Get(ctx, key, &pod); err != nil {
		t.Fatalf("session pod not created: %v", err)
	}
	if pod.Spec.ServiceAccountName != "tenant-runtime" {
		t.Errorf("pod serviceAccountName: got %q want tenant-runtime", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod restartPolicy: got %q want Never", pod.Spec.RestartPolicy)
	}
	if rc := pod.Spec.RuntimeClassName; rc == nil || *rc != runtimeClass {
		t.Errorf("pod runtimeClassName: got %v want %q", rc, runtimeClass)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("pod automountServiceAccountToken: want false")
	}
	// The default-deny NetworkPolicy must exist alongside it.
	var np networkingv1.NetworkPolicy
	if err := k8sClient.Get(ctx, key, &np); err != nil {
		t.Fatalf("session NetworkPolicy not created: %v", err)
	}
}
