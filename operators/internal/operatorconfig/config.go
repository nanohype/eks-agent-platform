/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package operatorconfig loads runtime configuration from SSM Parameter
// Store at operator startup. Outputs from terraform/components/* are
// published to /eks-agent-platform/<cluster-name>/<component>/<key>; this package
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

	// ClusterName is the full EKS cluster name this operator serves (e.g.
	// dev-analytics). Injected at startup (AGENTS_CLUSTER_NAME) — it is the key
	// of this cluster's SSM subtree and the prefix of every resource name the
	// operator mints (tenant/session roles), so co-located sibling clusters in
	// one account never collide. Also the cluster the operator creates tenant
	// Pod Identity associations on.
	ClusterName string

	// agent-iam outputs
	OperatorRoleARN              string
	TenantIAMPath                string
	TenantBaselinePolicyARN      string
	TenantPermissionsBoundaryARN string

	// model-artifacts outputs
	ArtifactsBucketName string

	// bedrock outputs. The invocation log group and its bucket are deliberately
	// absent: the operator neither reads Bedrock invocation logs nor addresses the
	// bucket. The log group's consumer is cost-pipeline's subscription filter, which
	// reads it from the account contract at /eks-agent-platform/org/bedrock-account/.
	BaselineGuardrailID      string
	BaselineGuardrailVersion string

	// kill-switch outputs
	KillSwitchEventBusName string

	// managed-monitoring outputs. AMPEndpoint is the Amazon Managed Prometheus
	// workspace base URL the SLO reconciler queries. Deliberately absent from
	// Validate's required set: cluster-bootstrap's enable_managed_monitoring
	// defaults false, so most clusters publish no such parameter, and requiring
	// it would refuse to start the operator on every one of them. The SLO
	// reconciler degrades to a MetricStoreUnavailable condition instead.
	AMPEndpoint string

	// cost-pipeline outputs. Exactly the three the Budget reconciler's query gate
	// requires — it refuses to build a query with any of them empty. The CUR bucket
	// and the Athena results bucket are not among them: the query addresses the CUR
	// through the Glue catalog rather than through S3, and the results location is
	// enforced by the workgroup, so a client-supplied one would be ignored. Both
	// buckets are still reached at runtime, under the operator's own identity, by
	// grants cost-access mints — held in IAM, not in this struct.
	AthenaWorkgroup string
	AthenaDatabase  string
	CURTableName    string

	// eval-runtime outputs
	EvalRunnerNamespace      string
	EvalRunnerServiceAccount string
	EvalReportsBucket        string

	// batch-runtime outputs
	BatchServiceRoleARN string
}

// Load fetches every parameter under /eks-agent-platform/<cluster-name>/
// in a single GetParametersByPath sweep (pagination-aware) and decodes
// the well-known keys into a Config. Unknown keys are ignored — adding a
// new SSM output is non-breaking. The SSM subtree is keyed by the full
// cluster name so co-located sibling clusters get isolated substrates.
func Load(ctx context.Context, ssmClient awsclients.SSM, clusterName, environment, region string) (*Config, error) {
	if clusterName == "" {
		return nil, fmt.Errorf("operatorconfig: cluster name is required")
	}
	cfg := &Config{ClusterName: clusterName, Environment: environment, Region: region}
	prefix := "/eks-agent-platform/" + clusterName + "/"

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
	case "model-artifacts/bucket_name":
		c.ArtifactsBucketName = value
	case "bedrock/baseline_guardrail_id":
		c.BaselineGuardrailID = value
	case "bedrock/baseline_guardrail_version":
		c.BaselineGuardrailVersion = value
	case "kill-switch/event_bus_name":
		c.KillSwitchEventBusName = value
	case "managed-monitoring/amp_endpoint":
		c.AMPEndpoint = value
	case "cost-pipeline/athena_workgroup":
		c.AthenaWorkgroup = value
	case "cost-pipeline/athena_database":
		c.AthenaDatabase = value
	case "cost-pipeline/cur_table_name":
		c.CURTableName = value
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

// Validate returns a list of required-but-missing field names. The required
// set is the fields whose absence makes tenant IAM reconciliation unsafe
// rather than merely degraded — in particular TenantPermissionsBoundaryARN:
// without it the operator would mint tenant roles with no permissions
// boundary, silently. A non-empty result must abort startup (see cmd/main.go);
// optional integrations (guardrails, kill-switch, cost pipeline) are not in
// this set and degrade per-reconciler instead.
func (c *Config) Validate() []string {
	missing := []string{}
	required := map[string]string{
		"OperatorRoleARN":              c.OperatorRoleARN,
		"TenantIAMPath":                c.TenantIAMPath,
		"TenantBaselinePolicyARN":      c.TenantBaselinePolicyARN,
		"TenantPermissionsBoundaryARN": c.TenantPermissionsBoundaryARN,
		"ArtifactsBucketName":          c.ArtifactsBucketName,
	}
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

func ptrBool(b bool) *bool { return &b }
