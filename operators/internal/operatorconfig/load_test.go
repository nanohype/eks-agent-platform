/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package operatorconfig

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fakeSSM is an in-memory awsclients.SSM. It serves GetParametersByPath from a
// map, optionally across two pages, and can inject an error. GetParameter is
// unused by Load and panics if called, so a new call site is loud.
type fakeSSM struct {
	params    map[string]string
	err       error
	paginate  bool // serve the params across two pages
	pageCalls int
}

func (f *fakeSSM) GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	panic("GetParameter not used by Load")
}

func (f *fakeSSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	prefix := aws.ToString(in.Path)
	keys := make([]string, 0, len(f.params))
	for k := range f.params {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable order so the two-page split is consistent across calls
	all := make([]ssmtypes.Parameter, 0, len(keys))
	for _, k := range keys {
		all = append(all, ssmtypes.Parameter{Name: aws.String(prefix + k), Value: aws.String(f.params[k])})
	}
	if !f.paginate {
		return &ssm.GetParametersByPathOutput{Parameters: all}, nil
	}
	f.pageCalls++
	half := len(all) / 2
	if in.NextToken == nil {
		return &ssm.GetParametersByPathOutput{Parameters: all[:half], NextToken: aws.String("next")}, nil
	}
	return &ssm.GetParametersByPathOutput{Parameters: all[half:]}, nil
}

func TestLoad_DecodesEveryKnownKeyAcrossPages(t *testing.T) {
	// One representative key per component, plus an unknown key that must be
	// ignored — proves Load's single sweep + decode wires each SSM output into
	// the right Config field.
	f := &fakeSSM{
		paginate: true,
		params: map[string]string{
			"agent-iam/operator_role_arn":               "arn:op",
			"agent-iam/tenant_iam_path":                 "/agents/",
			"agent-iam/tenant_baseline_policy_arn":      "arn:baseline",
			"agent-iam/tenant_permissions_boundary_arn": "arn:boundary",
			"model-artifacts/bucket_name":               "artifacts",
			"model-artifacts/bucket_arn":                "arn:artifacts",
			"model-artifacts/eval_reports_bucket_arn":   "arn:eval",
			"model-artifacts/eval_reports_bucket_name":  "eval",
			"bedrock/baseline_guardrail_id":             "gr-1",
			"bedrock/baseline_guardrail_version":        "3",
			"bedrock/invocation_bucket_arn":             "arn:inv",
			"bedrock/invocation_log_group":              "/logs/inv",
			"kill-switch/event_bus_name":                "bus",
			"kill-switch/event_bus_arn":                 "arn:bus",
			"kill-switch/state_machine_arn":             "arn:sfn",
			"cost-pipeline/cur_bucket":                  "cur",
			"cost-pipeline/athena_workgroup":            "wg",
			"cost-pipeline/athena_database":             "db",
			"cost-pipeline/athena_results_bucket":       "res",
			"cost-pipeline/cur_table_name":              "cur_table",
			"eval-runtime/runner_role_arn":              "arn:runner",
			"eval-runtime/runner_namespace":             "evals",
			"eval-runtime/runner_service_account":       "eval-runner",
			"eval-runtime/eval_reports_bucket":          "eval-reports",
			"batch-runtime/service_role_arn":            "arn:batch",
			"unrecognized/key":                          "ignored",
		},
	}
	cfg, err := Load(context.Background(), f, "dev-analytics", "development", "us-west-2")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.pageCalls < 2 {
		t.Errorf("expected pagination to make two calls, got %d", f.pageCalls)
	}
	checks := map[string]string{
		"ClusterName":                  cfg.ClusterName,
		"OperatorRoleARN":              cfg.OperatorRoleARN,
		"TenantIAMPath":                cfg.TenantIAMPath,
		"TenantBaselinePolicyARN":      cfg.TenantBaselinePolicyARN,
		"TenantPermissionsBoundaryARN": cfg.TenantPermissionsBoundaryARN,
		"ArtifactsBucketName":          cfg.ArtifactsBucketName,
		"ArtifactsBucketARN":           cfg.ArtifactsBucketARN,
		"EvalReportsBucketARN":         cfg.EvalReportsBucketARN,
		"EvalReportsBucketName":        cfg.EvalReportsBucketName,
		"BaselineGuardrailID":          cfg.BaselineGuardrailID,
		"BaselineGuardrailVersion":     cfg.BaselineGuardrailVersion,
		"InvocationBucketARN":          cfg.InvocationBucketARN,
		"InvocationLogGroupName":       cfg.InvocationLogGroupName,
		"KillSwitchEventBusName":       cfg.KillSwitchEventBusName,
		"KillSwitchEventBusARN":        cfg.KillSwitchEventBusARN,
		"StateMachineARN":              cfg.StateMachineARN,
		"CURBucketName":                cfg.CURBucketName,
		"AthenaWorkgroup":              cfg.AthenaWorkgroup,
		"AthenaDatabase":               cfg.AthenaDatabase,
		"AthenaResultsBucket":          cfg.AthenaResultsBucket,
		"CURTableName":                 cfg.CURTableName,
		"EvalRunnerRoleARN":            cfg.EvalRunnerRoleARN,
		"EvalRunnerNamespace":          cfg.EvalRunnerNamespace,
		"EvalRunnerServiceAccount":     cfg.EvalRunnerServiceAccount,
		"EvalReportsBucket":            cfg.EvalReportsBucket,
		"BatchServiceRoleARN":          cfg.BatchServiceRoleARN,
	}
	for field, got := range checks {
		if got == "" {
			t.Errorf("Config.%s not populated from SSM", field)
		}
	}
	if cfg.Environment != "development" || cfg.Region != "us-west-2" {
		t.Errorf("env/region: got %q/%q", cfg.Environment, cfg.Region)
	}
}

func TestLoad_RequiresClusterName(t *testing.T) {
	if _, err := Load(context.Background(), &fakeSSM{}, "", "development", "us-west-2"); err == nil {
		t.Fatal("Load must reject an empty cluster name (it keys the SSM subtree)")
	}
}

func TestLoad_PropagatesSSMError(t *testing.T) {
	f := &fakeSSM{err: errors.New("access denied")}
	if _, err := Load(context.Background(), f, "dev-analytics", "development", "us-west-2"); err == nil {
		t.Fatal("a GetParametersByPath error must abort Load")
	}
}

func TestLoad_IgnoresNilNameOrValueParameters(t *testing.T) {
	// A parameter with a nil Name or Value must be skipped, not panic.
	f := &nilParamSSM{}
	if _, err := Load(context.Background(), f, "dev-analytics", "development", "us-west-2"); err != nil {
		t.Fatalf("Load must tolerate nil-field parameters: %v", err)
	}
}

// nilParamSSM returns one well-formed parameter and two malformed ones (nil
// Name, nil Value) so the skip branch in Load is exercised.
type nilParamSSM struct{}

func (nilParamSSM) GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	panic("unused")
}

func (nilParamSSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	prefix := aws.ToString(in.Path)
	return &ssm.GetParametersByPathOutput{Parameters: []ssmtypes.Parameter{
		{Name: aws.String(prefix + "agent-iam/operator_role_arn"), Value: aws.String("arn:op")},
		{Name: nil, Value: aws.String("orphan")},
		{Name: aws.String(prefix + "x"), Value: nil},
	}}, nil
}
