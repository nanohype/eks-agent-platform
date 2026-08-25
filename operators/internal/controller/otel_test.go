/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"
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

// The egress allow-list and the endpoint are two halves of one fact, and each is
// silent when the other is missing: a hole nothing is addressed at carries
// nothing, and an address with no hole is refused. They are asserted together
// because they were shipped apart — the allow-list was correct and tested while
// OTEL_EXPORTER_OTLP_ENDPOINT appeared nowhere in the tree, so every
// operator-built pod defaulted to localhost:4317 and dropped its telemetry with
// the pod Running and the collector healthy.
func TestOTelEndpointIsTheOneEgressAllows(t *testing.T) {
	endpoint := otelExporterEndpoint()

	// Derived from the same constants the NetworkPolicy rules use, so the
	// address a pod is given cannot drift from the address it may reach.
	if !strings.Contains(endpoint, collectorNamespace) {
		t.Errorf("endpoint %q does not name the collector namespace %q that egress allows", endpoint, collectorNamespace)
	}
	if !strings.Contains(endpoint, fmt.Sprint(otlpGRPCPort)) {
		t.Errorf("endpoint %q does not name the gRPC port %d that egress allows", endpoint, otlpGRPCPort)
	}

	// The neutral waist: the alias Service is deliberately not named for the
	// collector implementing it, so swapping collectors stays a cluster decision.
	if !strings.Contains(endpoint, "telemetry.") {
		t.Errorf("endpoint %q is not the neutral telemetry. alias", endpoint)
	}
	for _, backend := range []string{"otel-gateway", "alloy", "collector", "amp", "prometheus"} {
		if strings.Contains(endpoint, backend) {
			t.Errorf("endpoint %q names a collector or backend (%q); the workload must not know which one implements the alias", endpoint, backend)
		}
	}
}

// Endpoint and protocol are set from one place because a mismatch fails on every
// export with an error only the workload's own logs carry.
func TestOTelEndpointAndProtocolAgree(t *testing.T) {
	p := otelTestPlatform("anthropic")
	env := withOTelResourceAttrs(nil, p, "anthropic", "production")

	got := map[string]string{}
	for _, e := range env {
		got[e.Name] = e.Value
	}
	if got[otelEndpointEnvName] == "" {
		t.Fatal("no OTEL_EXPORTER_OTLP_ENDPOINT stamped; the SDK falls back to localhost and drops every span")
	}
	if got[otelProtocolEnvName] != "grpc" {
		t.Errorf("protocol = %q, want grpc", got[otelProtocolEnvName])
	}
	if got[otelProtocolEnvName] == "grpc" && !strings.HasSuffix(got[otelEndpointEnvName], fmt.Sprint(otlpGRPCPort)) {
		t.Errorf("protocol is grpc but the endpoint %q does not address the gRPC port", got[otelEndpointEnvName])
	}
}

// A tenant-supplied destination is dropped, not merged: the waist exists so a
// workload does not choose its backend, and a duplicate env key is undefined.
func TestTenantSuppliedOTelWiringIsOverridden(t *testing.T) {
	p := otelTestPlatform("anthropic")
	env := withOTelResourceAttrs([]corev1.EnvVar{
		{Name: otelEndpointEnvName, Value: "http://an-impostor-backend:4317"},
		{Name: otelProtocolEnvName, Value: "http/protobuf"},
		{Name: "KEEP_ME", Value: "1"},
	}, p, "anthropic", "production")

	var endpoints, protocols, kept int
	for _, e := range env {
		switch e.Name {
		case otelEndpointEnvName:
			endpoints++
			if strings.Contains(e.Value, "impostor") {
				t.Error("the tenant's own endpoint survived; the workload chose its backend")
			}
		case otelProtocolEnvName:
			protocols++
		case "KEEP_ME":
			kept++
		}
	}
	if endpoints != 1 || protocols != 1 {
		t.Errorf("endpoint=%d protocol=%d, want exactly one each — a duplicate env key is undefined behavior", endpoints, protocols)
	}
	if kept != 1 {
		t.Error("an unrelated tenant env var was dropped")
	}
}
