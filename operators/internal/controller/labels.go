/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

// Canonical k8s label keys for the objects the operator creates, under the
// reserved agents.nanohype.dev/* prefix — the resource-tagging standard's render
// for agent/tenant identity (and the namespace tenant-chart-base already uses).
//
// Each key is defined once and referenced for BOTH an object's metadata label
// AND any selector that matches it (NetworkPolicy podSelectors, the immutable
// Deployment/Service selectors). Sharing the constant makes a label and its
// selector physically unable to drift — the failure mode this file exists to
// prevent.
const labelPrefix = "agents.nanohype.dev"

// Exported label keys, one per kind of object the operator labels. Each is the
// single source of truth for both the object's metadata label and any selector
// that matches it.
// The platform.nanohype.dev/* dimensions the resource-tagging standard requires
// on every k8s object (required_by_surface.k8s), alongside the two
// app.kubernetes.io/* keys stamped in labelsForPlatform. They sit under a
// different prefix from the agents.nanohype.dev/* keys below because the
// standard renders them there — org dimensions under <group>.nanohype.dev, and
// tenant/agent identity under the agents group.
const (
	labelEnvironment = "platform.nanohype.dev/environment"
	labelTeam        = "platform.nanohype.dev/team"
)

// The label keys the operator stamps on everything it creates. They are exported
// because the tenant chart, the eval runner and the conformance suite select on
// exactly these strings, and a selector that disagrees with the stamp silently
// matches nothing.
const (
	LabelPlatform      = labelPrefix + "/platform"
	LabelTenant        = labelPrefix + "/tenant"
	LabelPersona       = labelPrefix + "/persona"
	LabelFleet         = labelPrefix + "/fleet"
	LabelAgent         = labelPrefix + "/agent"
	LabelAgentFleet    = labelPrefix + "/agent-fleet"
	LabelAgentSandbox  = labelPrefix + "/agentsandbox"
	LabelSandboxPool   = labelPrefix + "/sandboxpool"
	LabelMetricsBridge = labelPrefix + "/metrics-bridge"
	LabelEvalSuite     = labelPrefix + "/eval-suite"
	LabelPassThreshold = labelPrefix + "/pass-threshold"
	LabelModelFamily   = labelPrefix + "/model-family"
)
