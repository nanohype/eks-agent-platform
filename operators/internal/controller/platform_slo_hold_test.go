/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The AppProject is single-writer: ensureAppProject replaces the whole spec on
// every tick, so the rollout hold has to be part of what it renders rather than
// something another controller writes alongside it. These tests pin that the
// rendering follows SLOPolicy hold state in both directions — the property the
// SLO reconciler's effect verification depends on.

func sloPolicyWithHold(name, platform string, engaged bool) *governancev1alpha1.SLOPolicy {
	sp := &governancev1alpha1.SLOPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenants-acme"},
		Spec: governancev1alpha1.SLOPolicySpec{
			PlatformRef:      commonv1alpha1.LocalRef{Name: platform},
			SLI:              governancev1alpha1.SLI{Type: sliTypeAvailability, Metric: "acme_api"},
			Objective:        "0.999",
			OnPageTierBreach: onBreachHoldRollout,
		},
	}
	if engaged {
		ts := metav1.NewTime(time.Now())
		sp.Status.HoldEngagedAt = &ts
	}
	return sp
}

func newHoldPlatform() *platformv1alpha1.Platform {
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: ctrlTestPlatform, Namespace: "tenants-acme"},
		Spec:       platformv1alpha1.PlatformSpec{Tenant: ctrlTestPlatform},
	}
}

func newHoldReconciler(t *testing.T, objs ...client.Object) *PlatformReconciler {
	t.Helper()
	s := sloTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&governancev1alpha1.SLOPolicy{}).
		Build()
	return &PlatformReconciler{Client: cl, Scheme: s}
}

func TestSLOHoldWindows(t *testing.T) {
	p := newHoldPlatform()

	cases := []struct {
		name    string
		policy  *governancev1alpha1.SLOPolicy
		wantLen int
	}{
		{"no hold engaged", sloPolicyWithHold("acme-slo", ctrlTestPlatform, false), 0},
		{"hold engaged", sloPolicyWithHold("acme-slo", ctrlTestPlatform, true), 1},
		// A policy in the same namespace pointing at a different Platform must
		// not hold this one — tenants share a namespace only by accident, but a
		// cross-tenant hold would be a silent outage.
		{"engaged hold for another platform", sloPolicyWithHold("other-slo", "other", true), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newHoldReconciler(t, p, c.policy)
			got, err := r.sloHoldWindows(context.Background(), p)
			if err != nil {
				t.Fatalf("sloHoldWindows: %v", err)
			}
			if len(got) != c.wantLen {
				t.Fatalf("rendered %d windows, want %d", len(got), c.wantLen)
			}
			if c.wantLen > 0 && !isSLOHoldWindow(got[0].(map[string]interface{})) {
				t.Errorf("rendered window is not the one the SLO reconciler looks for: %v", got[0])
			}
		})
	}
}

// One deny window already denies everything in the project, so a second engaged
// policy must not stack duplicates into the spec on every reconcile.
func TestSLOHoldWindowsDoesNotStackDuplicates(t *testing.T) {
	p := newHoldPlatform()
	r := newHoldReconciler(t, p,
		sloPolicyWithHold("acme-slo", ctrlTestPlatform, true),
		sloPolicyWithHold("acme-latency-slo", ctrlTestPlatform, true),
	)
	got, err := r.sloHoldWindows(context.Background(), p)
	if err != nil {
		t.Fatalf("sloHoldWindows: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("two engaged holds must render one window, got %d", len(got))
	}
}

func readSyncWindows(t *testing.T, r *PlatformReconciler, name string) []interface{} {
	t.Helper()
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(appProjectGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: argoCDNamespace, Name: name}, ap); err != nil {
		t.Fatalf("get AppProject: %v", err)
	}
	windows, _, err := unstructured.NestedSlice(ap.Object, "spec", "syncWindows")
	if err != nil {
		t.Fatalf("read syncWindows: %v", err)
	}
	return windows
}

// The round trip that matters: engaging a hold puts the window on the
// AppProject, and clearing it takes the window off — both through the one writer.
func TestEnsureAppProjectRendersAndRemovesTheHold(t *testing.T) {
	p := newHoldPlatform()
	sp := sloPolicyWithHold("acme-slo", ctrlTestPlatform, true)
	r := newHoldReconciler(t, p, sp)

	if err := r.ensureAppProject(context.Background(), p); err != nil {
		t.Fatalf("ensureAppProject with an engaged hold: %v", err)
	}
	windows := readSyncWindows(t, r, ctrlTestPlatform)
	if len(windows) != 1 || !isSLOHoldWindow(windows[0].(map[string]interface{})) {
		t.Fatalf("an engaged hold must render a deny syncWindow, got %v", windows)
	}

	// Clear the hold and reconcile again. This is the path that would silently
	// leave a tenant blocked forever if the spec were only ever added to.
	sp.Status.HoldEngagedAt = nil
	if err := r.Status().Update(context.Background(), sp); err != nil {
		t.Fatalf("clear hold: %v", err)
	}
	if err := r.ensureAppProject(context.Background(), p); err != nil {
		t.Fatalf("ensureAppProject after release: %v", err)
	}
	if windows := readSyncWindows(t, r, ctrlTestPlatform); len(windows) != 0 {
		t.Errorf("releasing the hold must remove the deny syncWindow, got %v", windows)
	}
}

// A Platform with no SLOPolicy at all must render an AppProject with no
// syncWindows key — the overwhelmingly common case, and the one where an
// accidental empty-list key would show up as spurious drift in ArgoCD.
func TestEnsureAppProjectOmitsSyncWindowsWithoutAHold(t *testing.T) {
	p := newHoldPlatform()
	r := newHoldReconciler(t, p)
	if err := r.ensureAppProject(context.Background(), p); err != nil {
		t.Fatalf("ensureAppProject: %v", err)
	}
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(appProjectGVK)
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: argoCDNamespace, Name: ctrlTestPlatform}, ap); err != nil {
		t.Fatalf("get AppProject: %v", err)
	}
	if _, found, _ := unstructured.NestedSlice(ap.Object, "spec", "syncWindows"); found {
		t.Error("an unheld tenant's AppProject must carry no syncWindows key at all")
	}
}

// The mapper is what makes a hold land promptly instead of waiting for a resync
// the operator may not even be running.
func TestSLOPolicyToPlatformMapping(t *testing.T) {
	got := sloPolicyToPlatform(context.Background(), sloPolicyWithHold("acme-slo", ctrlTestPlatform, true))
	if len(got) != 1 {
		t.Fatalf("want one reconcile request, got %d", len(got))
	}
	if got[0].Namespace != "tenants-acme" || got[0].Name != "acme" {
		t.Errorf("mapped to %s/%s, want tenants-acme/acme", got[0].Namespace, got[0].Name)
	}

	if r := sloPolicyToPlatform(context.Background(), &platformv1alpha1.Platform{}); r != nil {
		t.Errorf("a non-SLOPolicy object must map to nothing, got %v", r)
	}
	if r := sloPolicyToPlatform(context.Background(), &governancev1alpha1.SLOPolicy{}); r != nil {
		t.Errorf("an SLOPolicy with no platformRef must map to nothing, got %v", r)
	}
}
