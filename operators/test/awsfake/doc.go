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
// Each fake refuses the calls listed here, with the error the SDK hands back
// for each, so a test fails where AWS would turn a reconciler's call down for
// one of these reasons. A call AWS refuses for a reason not listed here
// passes.
//
// IAM refuses a role name over 64 characters, a name already taken, and a
// policy document that is not a JSON object or breaks the policy grammar's
// structure: a Version IAM does not accept, no Statement, or a statement
// without an Effect of Allow or Deny, an Action or NotAction, and the
// Resource or Principal its kind of document requires, or with the one its
// kind forbids (documents.go has the rule). It does not read condition
// operators, ARNs or action names. It
// refuses a trust policy over its 2,048-character default quota, a role's
// inline policies over 10,240 characters together, any call naming a role
// that does not exist, a detach or inline delete of a policy the role does
// not hold, and DeleteRole while the role holds policies. It returns trust
// and inline documents URL-encoded, as IAM does.
//
// EKS refuses a second Pod Identity association for one ServiceAccount and an
// association id it does not hold. Athena answers a repeated
// ClientRequestToken with the first execution's id, and refuses one repeated
// with any parameter changed. EventBridge refuses a PutEvents call outside
// 1-10 entries and fails each entry missing its Source, DetailType or a
// JSON-object Detail. Scheduler refuses to create a schedule group that
// already exists and answers ResourceNotFound for one it does not hold.
// CloudWatch answers GetMetricData from a script and refuses nothing.
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
