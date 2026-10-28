/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import "testing"

// TestEvalRuntimeOutputsReachThePlatformReconciler: the Platform reconciler
// mints each eval identity from the same two eval-runtime outputs the EvalSuite
// reconciler submits runs with. Dropping either across the mapping leaves runs
// submitted to a namespace, or uploading to a bucket, the identity was not
// minted for.
func TestEvalRuntimeOutputsReachThePlatformReconciler(t *testing.T) {
	cfg := loadWith(t, map[string]string{
		"eval-runtime/runner_namespace":    "evals",
		"eval-runtime/eval_reports_bucket": "reports",
	})
	got := evalIdentityConfigFrom(cfg)
	if got.Namespace != cfg.EvalRunnerNamespace || got.Namespace != "evals" {
		t.Errorf("Namespace = %q, want the published runner_namespace %q", got.Namespace, "evals")
	}
	if got.ReportsBucket != cfg.EvalReportsBucket || got.ReportsBucket != "reports" {
		t.Errorf("ReportsBucket = %q, want the published eval_reports_bucket %q", got.ReportsBucket, "reports")
	}
}
