/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package awsfake holds the in-memory AWS clients the operator's tests inject
// in place of the aws-sdk-go-v2 clients behind internal/awsclients: IAM, EKS,
// Athena, EventBridge, CloudWatch and EventBridge Scheduler. The controller
// package's unit tests and the envtest conformance suite share them, so a
// behaviour pinned in one suite is the behaviour the other runs against.
//
// The fakes are strict: each refuses what the service it stands in for
// refuses, with the error the SDK hands back for it, so a reconciler cannot
// pass against a fake that accepted a call AWS would have turned down. IAM
// refuses a role name over 64 characters, a name already taken, a policy
// document that is not a JSON object, a trust policy over its 2,048-character
// default quota, a role's inline policies over 10,240 characters together, any
// call naming a role that does not exist, a detach or inline delete of a
// policy the role does not hold, and DeleteRole while the role holds policies.
// It returns trust and inline documents URL-encoded, as IAM does. EKS refuses
// a second Pod Identity association for one ServiceAccount and an association
// id it does not hold. Athena answers a repeated ClientRequestToken with the
// first execution's id. EventBridge refuses a PutEvents call outside 1-10
// entries and fails each entry missing its Source, DetailType or a JSON-object
// Detail. Scheduler refuses a schedule group that already exists.
//
// Strictness is the default; the exported knob fields inject what a test needs
// beyond it (an error on one call, AccessDenied where IAM authorizes against a
// name, a page size). The state and recorder fields are exported too, so a
// test seeds and asserts on them directly.
//
// Every method holds the fake's lock, so a fake is safe to hand to a
// reconciler running in its own goroutine. A test reading or writing the
// exported fields while one might be running holds the lock as well:
// f.Lock(); defer f.Unlock().
//
// The package sits outside internal/ on purpose. The coverage gate measures
// -coverpkg=./internal/..., and a fake is not code under test, so its
// statements stay out of every floor.
package awsfake
