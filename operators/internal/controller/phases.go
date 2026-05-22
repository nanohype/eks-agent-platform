/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

// Reconciler-wide phase string constants. Used by every status update so
// dashboards + CLI tools can join on stable values.
const (
	phasePending      = "Pending"
	phaseProvisioning = "Provisioning"
	phaseRunning      = "Running"
	phaseReady        = "Ready"
	phaseSucceeded    = "Succeeded"
	phaseSuspended    = "Suspended"
	phaseFailed       = "Failed"
)

// reasonPlatformSuspended is the status-condition reason set on tenant
// workloads when the Platform kill-switch has fired.
const reasonPlatformSuspended = "PlatformSuspended"
