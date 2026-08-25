/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

func otelTestPlatform(families ...string) *platformv1alpha1.Platform {
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api"},
		Spec: platformv1alpha1.PlatformSpec{
			Tenant:   "acme",
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: families},
		},
	}
}

func TestOtelResourceAttrs(t *testing.T) {
	p := otelTestPlatform("anthropic")

	// Required attributes only (no pinned family, no environment).
	if got, want := otelResourceAttrs(otelTestPlatform(), "", ""), "agents.tenant=acme,agents.platform=orders-api"; got != want {
		t.Errorf("required-only: got %q want %q", got, want)
	}
	// Model family appended (AI workload with a pinned family).
	if got, want := otelResourceAttrs(p, "anthropic", ""), "agents.tenant=acme,agents.platform=orders-api,agents.model_family=anthropic"; got != want {
		t.Errorf("with family: got %q want %q", got, want)
	}
	// deployment.environment is the resource-tagging render of the environment
	// dimension, and the operator supplies it — a Platform cannot.
	if got, want := otelResourceAttrs(p, "anthropic", "production"),
		"agents.tenant=acme,agents.platform=orders-api,deployment.environment=production,agents.model_family=anthropic"; got != want {
		t.Errorf("with environment: got %q want %q", got, want)
	}
	// An operator running without --environment omits the attribute rather than
	// emitting it blank: an attribute present with no value still matches a
	// query filtering on it, and returns a series belonging to no environment.
	if got := otelResourceAttrs(p, "anthropic", ""); strings.Contains(got, "deployment.environment") {
		t.Errorf("empty environment was emitted anyway: %q", got)
	}
}

func TestWithOTelResourceAttrs(t *testing.T) {
	p := otelTestPlatform("anthropic")
	base := []corev1.EnvVar{
		{Name: "ANTHROPIC_ENVIRONMENT_ID", Value: "env_1"},
		// A tenant-supplied OTEL_RESOURCE_ATTRIBUTES must be overridden, not
		// duplicated — the operator is authoritative for the attribution.
		{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: "agents.tenant=impostor"},
	}

	got := withOTelResourceAttrs(base, p, "anthropic", "production")

	// Exactly one OTEL_RESOURCE_ATTRIBUTES entry, and it carries the operator's
	// authoritative value.
	var count int
	var value string
	for _, e := range got {
		if e.Name == "OTEL_RESOURCE_ATTRIBUTES" {
			count++
			value = e.Value
		}
	}
	if count != 1 {
		t.Fatalf("OTEL_RESOURCE_ATTRIBUTES count: got %d want 1", count)
	}
	if want := "agents.tenant=acme,agents.platform=orders-api,deployment.environment=production,agents.model_family=anthropic"; value != want {
		t.Errorf("value: got %q want %q", value, want)
	}
	// The unrelated env var must survive.
	var kept bool
	for _, e := range got {
		if e.Name == "ANTHROPIC_ENVIRONMENT_ID" && e.Value == "env_1" {
			kept = true
		}
	}
	if !kept {
		t.Error("withOTelResourceAttrs dropped an unrelated env var")
	}
}

func TestPlatformModelFamily(t *testing.T) {
	cases := []struct {
		name     string
		families []string
		want     string
	}{
		{"single family is unambiguous", []string{"anthropic"}, "anthropic"},
		{"no families → omitted", nil, ""},
		{"multiple families → ambiguous, omitted", []string{"anthropic", "amazon-nova"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := platformModelFamily(otelTestPlatform(tc.families...)); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
