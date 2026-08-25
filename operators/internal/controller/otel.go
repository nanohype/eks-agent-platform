/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
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

// withOTelResourceAttrs returns env with a canonical OTEL_RESOURCE_ATTRIBUTES
// entry appended. Any pre-existing OTEL_RESOURCE_ATTRIBUTES (e.g. from a
// tenant-supplied AgentSandbox env) is dropped: the operator is authoritative
// for the tenant/platform attribution so dashboards can trust it, and a
// duplicate env key is undefined behavior in a container.
func withOTelResourceAttrs(env []corev1.EnvVar, p *platformv1alpha1.Platform, modelFamily, environment string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env)+1)
	for _, e := range env {
		if e.Name == otelResourceAttrsEnvName {
			continue
		}
		out = append(out, e)
	}
	return append(out, corev1.EnvVar{
		Name:  otelResourceAttrsEnvName,
		Value: otelResourceAttrs(p, modelFamily, environment),
	})
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
