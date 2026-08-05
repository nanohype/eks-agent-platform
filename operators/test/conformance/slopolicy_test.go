/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

// The SLI's metric name and selector are interpolated into a PromQL expression
// the operator signs with its own credentials, so their validation markers are a
// security control, not a nicety. These exercise them against a real API server
// — the only place the generated OpenAPI schema is actually enforced.

func newConformanceSLO(t *testing.T) *governancev1alpha1.SLOPolicy {
	t.Helper()
	return &governancev1alpha1.SLOPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "s"), Namespace: testNs},
		Spec: governancev1alpha1.SLOPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "conformance-platform"},
			SLI:         governancev1alpha1.SLI{Type: "availability", Metric: "checkout_api"},
			Objective:   "0.999",
		},
	}
}

func TestSLOPolicy_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	sp := newConformanceSLO(t)
	sp.Spec.SLI.Selector = map[string]string{"route": "/pay"}
	mustCreate(ctx, t, sp)

	var got governancev1alpha1.SLOPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sp.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Objective != "0.999" {
		t.Errorf("objective: got %q want %q", got.Spec.Objective, "0.999")
	}
	if got.Spec.SLI.Selector["route"] != "/pay" {
		t.Errorf("selector round-trip lost the route matcher: %v", got.Spec.SLI.Selector)
	}
	// The default is None, and that is the point rather than an omission.
	// HoldRollout writes a deny syncWindow to the AppProject named for the
	// Platform; a tenant synced under the shared `platform` project — which is
	// every ApplicationSet on the default path — has nothing for it to write
	// to. Defaulting to an action that cannot happen makes the API claim more
	// than it delivers, so holding is opt-in.
	if got.Spec.OnPageTierBreach != "None" {
		t.Errorf("onPageTierBreach defaulted to %q, want None", got.Spec.OnPageTierBreach)
	}
}

// Holding is still reachable — the default moved, the capability did not.
func TestSLOPolicy_AcceptsAnExplicitHold(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	sp := newConformanceSLO(t)
	sp.Spec.OnPageTierBreach = "HoldRollout"
	mustCreate(ctx, t, sp)

	var got governancev1alpha1.SLOPolicy
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sp.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.OnPageTierBreach != "HoldRollout" {
		t.Errorf("an explicitly requested hold did not survive admission: got %q", got.Spec.OnPageTierBreach)
	}
}

func TestSLOPolicy_AcceptsALatencyObjective(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	sp := newConformanceSLO(t)
	sp.Spec.SLI = governancev1alpha1.SLI{Type: "latency", Metric: "checkout_api", ThresholdSeconds: "0.5"}
	sp.Spec.Objective = "0.99"
	mustCreate(ctx, t, sp)
}

func TestSLOPolicy_RejectsInvalidSpecs(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	cases := []struct {
		name   string
		mutate func(*governancev1alpha1.SLOPolicy)
	}{
		{
			// The whole injection surface: anything but a bare Prometheus
			// identifier could reshape the query the operator signs.
			name:   "metric with PromQL punctuation",
			mutate: func(s *governancev1alpha1.SLOPolicy) { s.Spec.SLI.Metric = `x"} or up{` },
		},
		{
			name:   "unknown sli type",
			mutate: func(s *governancev1alpha1.SLOPolicy) { s.Spec.SLI.Type = "throughput" },
		},
		{
			// An objective of 1 leaves a zero error budget and an infinite burn
			// rate; the pattern admits only 0.x values.
			name:   "objective of one",
			mutate: func(s *governancev1alpha1.SLOPolicy) { s.Spec.Objective = "1" },
		},
		{
			name:   "objective above one",
			mutate: func(s *governancev1alpha1.SLOPolicy) { s.Spec.Objective = "1.5" },
		},
		{
			name:   "non-numeric objective",
			mutate: func(s *governancev1alpha1.SLOPolicy) { s.Spec.Objective = "three nines" },
		},
		{
			name:   "unknown breach action",
			mutate: func(s *governancev1alpha1.SLOPolicy) { s.Spec.OnPageTierBreach = "SuspendTenant" },
		},
		{
			name: "non-numeric latency threshold",
			mutate: func(s *governancev1alpha1.SLOPolicy) {
				s.Spec.SLI = governancev1alpha1.SLI{Type: "latency", Metric: "x", ThresholdSeconds: "half a second"}
			},
		},
		{
			// Admitted, it would build an invalid query on every tick forever —
			// a permanently failing objective that looks declared.
			name: "latency SLI with no threshold",
			mutate: func(s *governancev1alpha1.SLOPolicy) {
				s.Spec.SLI = governancev1alpha1.SLI{Type: "latency", Metric: "x"}
			},
		},
		{
			// Silently ignored, so the author believes they narrowed an
			// objective that is in fact measuring everything.
			name: "availability SLI carrying a latency threshold",
			mutate: func(s *governancev1alpha1.SLOPolicy) {
				s.Spec.SLI = governancev1alpha1.SLI{Type: "availability", Metric: "x", ThresholdSeconds: "0.5"}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp := newConformanceSLO(t)
			c.mutate(sp)
			if err := k8sClient.Create(ctx, sp); err == nil {
				t.Cleanup(func() { _ = k8sClient.Delete(ctx, sp) })
				t.Fatalf("the API server accepted %s; the validation marker is not doing its job", c.name)
			}
		})
	}
}
