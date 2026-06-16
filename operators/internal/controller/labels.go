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
