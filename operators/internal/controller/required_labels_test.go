/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"strings"
	"testing"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// requiredK8sLabels is resource-tagging's required_by_surface.k8s, verbatim.
//
// Held as a list here rather than derived, because a derivation from
// labelsForPlatform would assert the function against itself and pass for
// whatever it happens to stamp. The standard is the other party to this
// contract, so the standard's list is what the test carries.
var requiredK8sLabels = []string{
	"app.kubernetes.io/managed-by",
	"app.kubernetes.io/component",
	"platform.nanohype.dev/environment",
	"platform.nanohype.dev/team",
}

// Every object the operator creates in a tenant namespace is labelled from
// labelsForPlatform, so this is the one place the required set can be pinned
// for all of them at once. A missing dimension has no symptom: the object is
// created, the reconcile succeeds, and the cost or ownership rollup that groups
// on the label silently omits the tenant — a rollup cannot recover a dimension
// that was never stamped.
func TestLabelsForPlatformCarriesEveryRequiredDimension(t *testing.T) {
	r := &PlatformReconciler{IAMCfg: IAMConfig{Environment: "production"}}
	p := &platformv1alpha1.Platform{}
	p.Name = "orders-api"
	p.Spec.Tenant = "acme"
	p.Spec.Persona = "ops"

	got := r.labelsForPlatform(p)

	for _, key := range requiredK8sLabels {
		v, ok := got[key]
		if !ok {
			t.Errorf("required label %q is not stamped (resource-tagging required_by_surface.k8s)", key)
			continue
		}
		if v == "" {
			t.Errorf("required label %q is stamped empty, which the API server rejects", key)
		}
	}

	if got["platform.nanohype.dev/team"] != "acme" {
		t.Errorf("team = %q, want the owning tenant", got["platform.nanohype.dev/team"])
	}
	if got["platform.nanohype.dev/environment"] != "production" {
		t.Errorf("environment = %q, want the operator's", got["platform.nanohype.dev/environment"])
	}
}

// A label VALUE may not be empty on the API server's rules, and the operator
// runs without --environment on a dev cluster. Dropping the key is the
// recoverable failure — the object reconciles and can be re-labelled; stamping
// "" is rejected at admission and the Platform does not reconcile at all.
func TestLabelsForPlatformOmitsRatherThanStampsEmpty(t *testing.T) {
	r := &PlatformReconciler{} // no environment configured
	p := &platformv1alpha1.Platform{}
	p.Name = "orders-api"
	p.Spec.Tenant = "acme"

	got := r.labelsForPlatform(p)

	for k, v := range got {
		if v == "" {
			t.Errorf("label %q stamped with an empty value; the API server rejects the object", k)
		}
	}
	if _, ok := got["platform.nanohype.dev/environment"]; ok {
		t.Error("environment stamped despite none being configured")
	}
	// The dimensions that do not depend on operator config stay.
	if got["platform.nanohype.dev/team"] != "acme" {
		t.Error("team dropped along with the environment")
	}
}

// The reserved prefixes the standard names, so a new dimension lands under the
// right group rather than inventing a third namespace.
func TestLabelKeysUseAReservedPrefix(t *testing.T) {
	r := &PlatformReconciler{IAMCfg: IAMConfig{Environment: "production"}}
	p := &platformv1alpha1.Platform{}
	p.Name = "orders-api"
	p.Spec.Tenant = "acme"

	reserved := []string{
		"app.kubernetes.io/",
		"platform.nanohype.dev/",
		"agents.nanohype.dev/",
		"tenants.nanohype.dev/",
		"governance.nanohype.dev/",
	}
	for key := range r.labelsForPlatform(p) {
		var ok bool
		for _, prefix := range reserved {
			if strings.HasPrefix(key, prefix) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("label %q uses no reserved prefix (resource-tagging reserved_prefixes.k8s)", key)
		}
	}
}
