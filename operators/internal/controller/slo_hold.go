/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The rollout hold is an ArgoCD deny sync window on the tenant's AppProject.
//
// Why the AppProject and not the tenant's Applications: those are rendered by an
// ApplicationSet in eks-gitops, which owns them and reverts a foreign edit on its
// next reconcile — stripping syncPolicy.automated there is a fight the operator
// loses within minutes. The AppProject is operator-owned, and ArgoCD applies a
// project's deny windows to every Application in it: one write, no ownership
// contest, whole-tenant scope.
//
// Why this file only READS it: PlatformReconciler.ensureAppProject rewrites the
// entire AppProject spec on every tick (unstructured.SetNestedField assigns
// wholesale at the leaf, and controllerutil.CreateOrUpdate issues a full Update
// rather than a patch), so a second writer is erased within one requeue —
// deterministically, not occasionally. Rather than have two controllers fight
// over one field, the Platform reconciler renders the window from this
// reconciler's status (see sloHoldWindows in platform_reconcile.go) and stays the
// AppProject's only writer. This reconciler decides and then verifies, which is
// the same publish-then-confirm-the-effect shape the budget kill-switch uses:
// engaging a hold is not success, the window being present is.
//
// Why this schedule and duration: ArgoCD decides a window is active with
// schedule.Next(now - duration).Before(now). An hourly occurrence with a
// two-hour duration therefore always satisfies it — the next occurrence after
// (now - 2h) is at most an hour later, which is still in the past — so the
// window holds continuously until the entry is removed. A 24h duration on a
// daily schedule would leave a seam at the boundary; an every-minute schedule
// also works but churns ArgoCD's window evaluation for nothing.
const (
	sloHoldSchedule = "0 * * * *"
	sloHoldDuration = "2h"
	sloHoldTimeZone = "UTC"
)

// sloHoldDefaultGraceIntervals is how many reconcile intervals a hold may go
// unobserved before the reconciler calls it broken. Two is generous: the
// Platform reconciler watches SLOPolicy, so the window normally lands before the
// next tick reads it back.
const sloHoldDefaultGraceIntervals = 2

// appProjectGVK is the ArgoCD AppProject kind, addressed as unstructured for the
// same reason ensureAppProject does: to keep the argoproj.io Go types out of the
// operator's dependency graph.
var appProjectGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "AppProject",
}

// sloDenyWindow is the window the Platform reconciler renders for an engaged
// hold. manualSync stays true on purpose: the hold exists to stop automation
// from advancing a burning rollout, not to lock an on-call engineer out of
// shipping the fix. It is deliberately not configurable — a knob here would let
// someone lock their own responders out.
func sloDenyWindow() map[string]interface{} {
	return map[string]interface{}{
		"kind":         "deny",
		"schedule":     sloHoldSchedule,
		"duration":     sloHoldDuration,
		"timeZone":     sloHoldTimeZone,
		"applications": []interface{}{"*"},
		"manualSync":   true,
	}
}

// isSLOHoldWindow identifies a window rendered by sloDenyWindow.
//
// Matched on the subset of fields the operator sets rather than by deep equality:
// if ArgoCD ever defaults an extra field onto the window, deep equality would
// stop recognizing the operator's own write and light the "hold not landing"
// signal forever. A subset match cannot produce that false positive.
func isSLOHoldWindow(w map[string]interface{}) bool {
	if w["kind"] != "deny" || w["schedule"] != sloHoldSchedule || w["duration"] != sloHoldDuration {
		return false
	}
	apps, ok := w["applications"].([]interface{})
	if !ok || len(apps) != 1 {
		return false
	}
	return apps[0] == "*"
}

// holdObservation is what the effect check found.
type holdObservation struct {
	// present means the deny window is on the AppProject right now.
	present bool
	// unverifiable means there is no AppProject to inspect: the CRD is absent
	// (no ArgoCD on this cluster) or the tenant's project has not been created
	// yet. Distinct from "absent" so a cluster without ArgoCD does not read as a
	// broken hold.
	unverifiable bool
}

// observeHold reads the tenant's AppProject and reports whether the deny window
// is present. Read-only by design — see the file comment.
func (r *SLOReconciler) observeHold(ctx context.Context, platform *platformv1alpha1.Platform) (holdObservation, error) {
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(appProjectGVK)
	key := client.ObjectKey{Namespace: argoCDNamespace, Name: platform.Name}
	if err := r.Get(ctx, key, ap); err != nil {
		if isNoKindMatch(err) || apierrors.IsNotFound(err) {
			return holdObservation{unverifiable: true}, nil
		}
		return holdObservation{}, fmt.Errorf("get AppProject %s/%s: %w", argoCDNamespace, platform.Name, err)
	}
	windows, _, err := unstructured.NestedSlice(ap.Object, "spec", "syncWindows")
	if err != nil {
		return holdObservation{}, fmt.Errorf("read AppProject %s syncWindows: %w", platform.Name, err)
	}
	for _, raw := range windows {
		if w, ok := raw.(map[string]interface{}); ok && isSLOHoldWindow(w) {
			return holdObservation{present: true}, nil
		}
	}
	return holdObservation{}, nil
}

// holdGraceWindow is how long after engaging a hold the reconciler waits before
// declaring it unobserved: HoldGraceIntervals × RequeueInterval.
func (r *SLOReconciler) holdGraceWindow() time.Duration {
	n := r.HoldGraceIntervals
	if n <= 0 {
		n = sloHoldDefaultGraceIntervals
	}
	interval := r.RequeueInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return time.Duration(n) * interval
}

// holdUnobserved reports whether an engaged hold has failed to land. Pure over
// its inputs so the grace arithmetic is unit-testable without a cluster.
func (r *SLOReconciler) holdUnobserved(sp *governancev1alpha1.SLOPolicy, obs holdObservation, now time.Time) bool {
	if sp.Status.HoldEngagedAt == nil || obs.present || obs.unverifiable {
		return false
	}
	return now.Sub(sp.Status.HoldEngagedAt.Time) >= r.holdGraceWindow()
}
