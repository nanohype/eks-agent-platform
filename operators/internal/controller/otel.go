/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// otelResourceAttrsEnvName is the OpenTelemetry SDK env var every OTel exporter
// reads to seed resource attributes. The platform-tenant-contract requires
// agents.tenant + agents.platform on every pod (plus the model attributes for
// AI workloads); the operator stamps this env var on the pods it builds itself
// so its own workloads honor the same contract it holds tenants to.
const otelResourceAttrsEnvName = "OTEL_RESOURCE_ATTRIBUTES"

// The OTLP export destination, stamped on every pod the operator builds.
//
// Without it an OTel SDK falls back to localhost:4317, where nothing listens —
// and the export failure is invisible in the direction that matters: the pod
// stays Running, the collector stays healthy, and the series simply never
// arrive. That is the same silent-absence failure the collectorNamespace
// constant is written against, one layer up. The egress allow-list opening
// 4317/4318 to the collector namespace is necessary and not sufficient; a hole
// nothing is addressed at carries nothing.
//
// The endpoint is the telemetry-pipeline standard's neutral waist: a stable
// alias Service deliberately NOT named for the collector behind it, so swapping
// collectors is a cluster decision rather than an edit to every workload. The
// operator is authoritative for this the way it is for the resource attributes,
// and for the same reason — a workload that names its own destination has
// chosen a backend, which is precisely what the waist exists to prevent.
const (
	otelEndpointEnvName = "OTEL_EXPORTER_OTLP_ENDPOINT"
	otelProtocolEnvName = "OTEL_EXPORTER_OTLP_PROTOCOL"

	// Set together, from one place. An endpoint naming the gRPC port while the
	// SDK speaks HTTP/protobuf fails on every export with a protocol error the
	// workload's own logs carry and nothing else does, so the two must not be
	// able to disagree — the same coupling-over-checking shape as
	// internal/metricsbridge.
	otelProtocol = "grpc"
)

// otelExporterEndpoint is the OTLP destination for a pod on this cluster.
// Rendered from the same collectorNamespace and port constants the NetworkPolicy
// egress rules use, so the address a pod is given and the address it is allowed
// to reach cannot drift apart.
func otelExporterEndpoint() string {
	return fmt.Sprintf("http://telemetry.%s.svc.cluster.local:%d", collectorNamespace, otlpGRPCPort)
}

// otelResourceAttrs renders the platform-tenant-contract OTel resource
// attributes for a pod the operator creates. agents.tenant + agents.platform
// are always present (both required); agents.model_family is appended when the
// owning Platform pins a single family (AI workloads). agents.model_id is left
// unset — the operator-built pods (sandbox session, worker fleet, eval runner)
// resolve their model at request time rather than from a fixed spec, so no
// single model id is knowable when the pod is built. Values come straight from
// the owning Platform so cost/latency dashboards can slice by team and app.
//
// deployment.environment is the resource-tagging standard's OTel render of the
// environment dimension, and it is the one attribute here the Platform cannot
// supply: the environment is the operator's, not the tenant's. Omitted when the
// operator runs without one rather than emitted empty, because an attribute
// present with no value is worse than an absent one — a query filtering on it
// matches, and returns a series that belongs to no environment.
func otelResourceAttrs(p *platformv1alpha1.Platform, modelFamily, environment string) string {
	attrs := []string{
		"agents.tenant=" + p.Spec.Tenant,
		"agents.platform=" + p.Name,
	}
	if environment != "" {
		attrs = append(attrs, "deployment.environment="+environment)
	}
	if modelFamily != "" {
		attrs = append(attrs, "agents.model_family="+modelFamily)
	}
	return strings.Join(attrs, ",")
}

// withOTelResourceAttrs returns env carrying the operator's canonical OTel
// wiring: the resource attributes, the OTLP endpoint, and the protocol that
// endpoint's port speaks.
//
// Any pre-existing value for those three (e.g. from a tenant-supplied
// AgentSandbox env) is dropped rather than merged. The operator is authoritative
// for the attribution so dashboards can trust it; it is authoritative for the
// destination because the telemetry-pipeline waist means a workload does not
// choose its backend; and a duplicate env key is undefined behavior in a
// container, so "dropped" is the only option that is not a coin flip.
func withOTelResourceAttrs(env []corev1.EnvVar, p *platformv1alpha1.Platform, modelFamily, environment string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env)+3)
	for _, e := range env {
		if e.Name == otelResourceAttrsEnvName || e.Name == otelEndpointEnvName || e.Name == otelProtocolEnvName {
			continue
		}
		out = append(out, e)
	}
	return append(out,
		corev1.EnvVar{
			Name:  otelResourceAttrsEnvName,
			Value: otelResourceAttrs(p, modelFamily, environment),
		},
		corev1.EnvVar{Name: otelEndpointEnvName, Value: otelExporterEndpoint()},
		corev1.EnvVar{Name: otelProtocolEnvName, Value: otelProtocol},
	)
}

// platformModelFamily returns the Platform's model family when it pins exactly
// one — the only case where a single, unambiguous agents.model_family attribute
// is meaningful. Zero or many allowed families yield "" (attribute omitted).
func platformModelFamily(p *platformv1alpha1.Platform) string {
	if len(p.Spec.Identity.AllowedModelFamilies) == 1 {
		return p.Spec.Identity.AllowedModelFamilies[0]
	}
	return ""
}
