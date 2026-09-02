/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

// Two workloads the operator creates are collected only after they reach a
// terminal state, and neither reaches one on its own:
//
//	the AgentSandbox session pod   reconcileTTL counts from status.completedAt,
//	                              which is written when the pod goes terminal
//	the submitted eval run        the reconciler learns a run finished by
//	                              reading the workflow's phase
//
// So for each, something OUTSIDE this operator has to guarantee the terminal
// state arrives. That is what activeDeadlineSeconds is for on both, and it is
// the half that was never asserted: the sandbox tests cover the helper that
// computes the value and stop there, so deleting the field from the PodSpec
// left the whole suite green — unit and envtest alike.
//
// A test that reads the helper is testing arithmetic. These read the artifact,
// because the artifact is what the cluster enforces and the operator's absence
// is exactly the condition under which it has to.

// TestTheTTLCannotCollectASessionThatNeverEnds states the coupling both halves
// depend on, in one place, so neither can be read alone.
func TestTheTTLCannotCollectASessionThatNeverEnds(t *testing.T) {
	ctx := context.Background()
	box := &agentsv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hung", Namespace: ctrlTestNS},
		Spec:       agentsv1alpha1.AgentSandboxSpec{Image: "ghcr.io/acme/sandbox:v1"},
	}

	// A session that never terminates has no completedAt, and the TTL counts
	// from completedAt. The operator's own garbage collection is therefore not
	// merely slow here — it never starts.
	s := sandboxPoolScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentSandboxReconciler{Client: cl, Scheme: s}
	requeue, err := r.reconcileTTL(ctx, box)
	if err != nil {
		t.Fatalf("reconcileTTL on a session with no completedAt: %v", err)
	}
	if requeue != 0 {
		t.Errorf("reconcileTTL asked to requeue in %v for a session that never finished; "+
			"if this ever collects on its own, the reasoning below needs rewriting", requeue)
	}

	// Which is why the ceiling has to be on the pod, where the kubelet enforces
	// it whatever this operator is doing.
	deadline := sandboxCeilingFromCRDDefault(t)
	box.Spec.ActiveDeadlineSeconds = &deadline
	if err := r.ensureSessionPod(ctx, cl, box, newPlatform(ctrlTestPlatform, "team")); err != nil {
		t.Fatalf("ensureSessionPod: %v", err)
	}
	pod := sessionPod(t, cl, box)
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("the session pod carries no activeDeadlineSeconds. Nothing then bounds a hung session: " +
			"the pod holds its node slot and its tenant credentials until a human notices, and the TTL " +
			"above is waiting on a terminal phase that never arrives")
	}
	if got := *pod.Spec.ActiveDeadlineSeconds; got != int64(deadline) {
		t.Errorf("session pod activeDeadlineSeconds = %d, want %d — the ceiling the sandbox declared", got, deadline)
	}
}

// TestSessionCeilingReachesThePodForEveryDeclaration reads the artifact for each
// shape the field can take, rather than the helper that computes it.
func TestSessionCeilingReachesThePodForEveryDeclaration(t *testing.T) {
	secs := func(v int32) *int32 { return &v }
	cases := []struct {
		name string
		in   *int32
		want *int64
	}{
		{"a declared ceiling reaches the pod", secs(600), func() *int64 { v := int64(600); return &v }()},
		{"the shipped default reaches the pod", secs(sandboxCeilingFromCRDDefault(t)), func() *int64 {
			v := int64(sandboxCeilingFromCRDDefault(t))
			return &v
		}()},
		// Kubernetes rejects activeDeadlineSeconds: 0, so "disabled" has to be an
		// absent field rather than a zero one — and absent has to mean absent ON
		// THE POD, not merely nil out of the helper.
		{"an explicit zero leaves the field off the pod", secs(0), nil},
		{"unset leaves the field off the pod", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			box := &agentsv1alpha1.AgentSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: ctrlTestNS},
				Spec:       agentsv1alpha1.AgentSandboxSpec{Image: "ghcr.io/acme/sandbox:v1"},
			}
			box.Spec.ActiveDeadlineSeconds = tc.in

			s := sandboxPoolScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).Build()
			r := &AgentSandboxReconciler{Client: cl, Scheme: s}
			if err := r.ensureSessionPod(ctx, cl, box, newPlatform(ctrlTestPlatform, "team")); err != nil {
				t.Fatalf("ensureSessionPod: %v", err)
			}

			got := sessionPod(t, cl, box).Spec.ActiveDeadlineSeconds
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("pod carries activeDeadlineSeconds %d; the sandbox declared none", *got)
			case tc.want != nil && got == nil:
				t.Errorf("pod carries no activeDeadlineSeconds; the sandbox declared %d", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("pod activeDeadlineSeconds = %d, want %d", *got, *tc.want)
			}
		})
	}
}

// TestTheShippedSandboxDefaultIsACeiling reads the default out of the generated
// CRD rather than the Go marker, because the CRD is what the apiserver applies
// to a sandbox that declares nothing — which is most of them.
func TestTheShippedSandboxDefaultIsACeiling(t *testing.T) {
	if got := sandboxCeilingFromCRDDefault(t); got <= 0 {
		t.Errorf("the AgentSandbox CRD defaults activeDeadlineSeconds to %d; a sandbox that declares "+
			"nothing then runs unbounded, which is the shape a sandbox must not ship with", got)
	}
}

// TestSubmittedRunIsBoundedWithoutTheOperator reads the run the reconciler
// actually submits.
func TestSubmittedRunIsBoundedWithoutTheOperator(t *testing.T) {
	r := &EvalReconciler{
		RunnerNamespace:      "eval-runner",
		RunnerServiceAccount: "eval-runner-custom",
		ReportsBucket:        testReportsBucket,
	}
	wf := submitEvalWorkflow(t, r)

	// Argo's default is no deadline. With concurrencyPolicy Forbid on the
	// scheduled form, a run that never finishes is never overtaken and every
	// later run is skipped, while the suite keeps reporting the last completed
	// score — a suite that looks healthy and stopped being true.
	deadline, found, err := unstructured.NestedInt64(wf.Object, "spec", "activeDeadlineSeconds")
	if err != nil {
		t.Fatalf("read spec.activeDeadlineSeconds: %v", err)
	}
	if !found {
		t.Error("the submitted run carries no activeDeadlineSeconds; a hung run then blocks every " +
			"scheduled run after it while the suite's status still reports the last completed score")
	} else if deadline <= 0 {
		t.Errorf("activeDeadlineSeconds = %d, which bounds nothing", deadline)
	}

	// Finished runs are collected by Argo, not by a reconcile, for the same
	// reason: the collection has to happen while nobody is watching.
	for _, field := range []string{"secondsAfterSuccess", "secondsAfterFailure"} {
		ttl, found, err := unstructured.NestedInt64(wf.Object, "spec", "ttlStrategy", field)
		if err != nil {
			t.Fatalf("read spec.ttlStrategy.%s: %v", field, err)
		}
		if !found || ttl <= 0 {
			t.Errorf("the submitted run declares no ttlStrategy.%s; finished workflows accumulate "+
				"in the cluster with nothing collecting them", field)
		}
	}

	// A failure's pods are the only record of why it failed, so collection is on
	// success only. OnWorkflowCompletion would take the evidence with it.
	strategy, _, err := unstructured.NestedString(wf.Object, "spec", "podGC", "strategy")
	if err != nil {
		t.Fatalf("read spec.podGC.strategy: %v", err)
	}
	if strategy != "OnWorkflowSuccess" {
		t.Errorf("podGC.strategy = %q, want OnWorkflowSuccess — a completed-run strategy deletes the "+
			"pods of a FAILED run, which are the only record of why it failed", strategy)
	}
}

// sessionPod fetches the pod ensureSessionPod created.
func sessionPod(t *testing.T, cl client.Client, box *agentsv1alpha1.AgentSandbox) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	key := types.NamespacedName{
		Namespace: PlatformNamespace(newPlatform(ctrlTestPlatform, "team")),
		Name:      agentSandboxResourceName(box),
	}
	if err := cl.Get(context.Background(), key, pod); err != nil {
		t.Fatalf("session pod not created: %v", err)
	}
	return pod
}

// sandboxCeilingFromCRDDefault reads spec.activeDeadlineSeconds' default out of
// the generated AgentSandbox CRD — the artifact the apiserver applies, not the
// marker beside the Go field.
func sandboxCeilingFromCRDDefault(t *testing.T) int32 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "agents.nanohype.dev_agentsandboxes.yaml"))
	if err != nil {
		t.Fatalf("read the generated AgentSandbox CRD: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the generated AgentSandbox CRD: %v", err)
	}
	versions, _ := doc["spec"].(map[string]any)["versions"].([]any)
	for _, v := range versions {
		vm, _ := v.(map[string]any)
		if vm["name"] != "v1alpha1" {
			continue
		}
		schema, _ := vm["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
		spec, _ := schema["properties"].(map[string]any)["spec"].(map[string]any)
		field, ok := spec["properties"].(map[string]any)["activeDeadlineSeconds"].(map[string]any)
		if !ok {
			t.Fatal("the generated AgentSandbox CRD declares no spec.activeDeadlineSeconds")
		}
		def, ok := field["default"]
		if !ok {
			t.Fatal("spec.activeDeadlineSeconds carries no default; a sandbox that declares nothing runs unbounded")
		}
		n, ok := def.(float64)
		if !ok {
			t.Fatalf("spec.activeDeadlineSeconds default %v is not a number", def)
		}
		return int32(n)
	}
	t.Fatal("the generated AgentSandbox CRD declares no v1alpha1 schema")
	return 0
}
