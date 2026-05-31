/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package operatorconfig loads runtime configuration from SSM Parameter
// Store at operator startup. Outputs from terraform/components/* are
// published to /eks-agent-platform/<env>/<component>/<key>; this package
// fetches them once and caches them in the Config struct.
package operatorconfig

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// Config is the resolved set of cross-component values the operator needs
// to function. Populated by Load() once at startup; pass to reconcilers
// as a read-only struct.
type Config struct {
	Environment string
	Region      string

	// agent-iam outputs
	OperatorRoleARN              string
	TenantIAMPath                string
	TenantBaselinePolicyARN      string
	TenantPermissionsBoundaryARN string
	AllowedRegions               []string

	// model-artifacts outputs
	ArtifactsBucketARN    string
	ArtifactsBucketName   string
	EvalReportsBucketARN  string
	EvalReportsBucketName string

	// bedrock outputs
	BaselineGuardrailID      string
	BaselineGuardrailVersion string
	InvocationBucketARN      string
	InvocationLogGroupName   string

	// kill-switch outputs
	KillSwitchEventBusName string
	KillSwitchEventBusARN  string
	StateMachineARN        string

	// cost-pipeline outputs
	CURBucketName       string
	AthenaWorkgroup     string
	AthenaDatabase      string
	AthenaResultsBucket string
	CURTableName        string

	// eval-runtime outputs
	EvalRunnerRoleARN        string
	EvalRunnerNamespace      string
	EvalRunnerServiceAccount string
	EvalReportsBucket        string

	// batch-runtime outputs
	BatchServiceRoleARN string
}

// Load fetches every parameter under /eks-agent-platform/<environment>/
// in a single GetParametersByPath sweep (pagination-aware) and decodes
// the well-known keys into a Config. Unknown keys are ignored — adding a
// new SSM output is non-breaking.
func Load(ctx context.Context, ssmClient awsclients.SSM, environment, region string) (*Config, error) {
	if environment == "" {
		return nil, fmt.Errorf("operatorconfig: environment is required")
	}
	cfg := &Config{Environment: environment, Region: region}
	prefix := "/eks-agent-platform/" + environment + "/"

	var nextToken *string
	for {
		out, err := ssmClient.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:           &prefix,
			Recursive:      ptrBool(true),
			WithDecryption: ptrBool(true),
			NextToken:      nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("ssm GetParametersByPath %s: %w", prefix, err)
		}
		for _, p := range out.Parameters {
			if p.Name == nil || p.Value == nil {
				continue
			}
			cfg.assign(strings.TrimPrefix(*p.Name, prefix), *p.Value)
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		nextToken = out.NextToken
	}
	return cfg, nil
}

// assign routes a single SSM key (under the env prefix) into the right
// Config field. Unknown keys silently no-op — they aren't errors.
func (c *Config) assign(suffix, value string) {
	switch suffix {
	case "agent-iam/operator_role_arn":
		c.OperatorRoleARN = value
	case "agent-iam/tenant_iam_path":
		c.TenantIAMPath = value
	case "agent-iam/tenant_baseline_policy_arn":
		c.TenantBaselinePolicyARN = value
	case "agent-iam/tenant_permissions_boundary_arn":
		c.TenantPermissionsBoundaryARN = value
	case "model-artifacts/bucket_arn":
		c.ArtifactsBucketARN = value
	case "model-artifacts/bucket_name":
		c.ArtifactsBucketName = value
	case "model-artifacts/eval_reports_bucket_arn":
		c.EvalReportsBucketARN = value
	case "model-artifacts/eval_reports_bucket_name":
		c.EvalReportsBucketName = value
	case "bedrock/baseline_guardrail_id":
		c.BaselineGuardrailID = value
	case "bedrock/baseline_guardrail_version":
		c.BaselineGuardrailVersion = value
	case "bedrock/invocation_bucket_arn":
		c.InvocationBucketARN = value
	case "bedrock/invocation_log_group":
		c.InvocationLogGroupName = value
	case "kill-switch/event_bus_name":
		c.KillSwitchEventBusName = value
	case "kill-switch/event_bus_arn":
		c.KillSwitchEventBusARN = value
	case "kill-switch/state_machine_arn":
		c.StateMachineARN = value
	case "cost-pipeline/cur_bucket":
		c.CURBucketName = value
	case "cost-pipeline/athena_workgroup":
		c.AthenaWorkgroup = value
	case "cost-pipeline/athena_database":
		c.AthenaDatabase = value
	case "cost-pipeline/athena_results_bucket":
		c.AthenaResultsBucket = value
	case "cost-pipeline/cur_table_name":
		c.CURTableName = value
	case "eval-runtime/runner_role_arn":
		c.EvalRunnerRoleARN = value
	case "eval-runtime/runner_namespace":
		c.EvalRunnerNamespace = value
	case "eval-runtime/runner_service_account":
		c.EvalRunnerServiceAccount = value
	case "eval-runtime/eval_reports_bucket":
		c.EvalReportsBucket = value
	case "batch-runtime/service_role_arn":
		c.BatchServiceRoleARN = value
	}
}

// Validate returns a list of required-but-missing field names. Callers
// decide whether a partial config is fatal (production should fail-fast on
// missing tenant_baseline_policy_arn; dev may tolerate a missing baseline
// guardrail).
func (c *Config) Validate() []string {
	missing := []string{}
	required := map[string]string{
		"OperatorRoleARN":         c.OperatorRoleARN,
		"TenantIAMPath":           c.TenantIAMPath,
		"TenantBaselinePolicyARN": c.TenantBaselinePolicyARN,
		"ArtifactsBucketName":     c.ArtifactsBucketName,
	}
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

func ptrBool(b bool) *bool { return &b }
