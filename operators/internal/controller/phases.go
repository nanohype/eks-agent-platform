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
	phaseReady        = "Ready"
	phaseSuspended    = "Suspended"
	phaseFailed       = "Failed"
)
