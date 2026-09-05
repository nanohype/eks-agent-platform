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
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

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
	prefix := clusterPrefix(clusterName)

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
	// ssm-producer: landing-zone's agent-iam component, which owns this prefix.
	case "agent-iam/operator_role_arn":
		c.OperatorRoleARN = value
	// ssm-producer: landing-zone's agent-iam component, which owns this prefix.
	case "agent-iam/tenant_iam_path":
		c.TenantIAMPath = value
	// ssm-producer: landing-zone's agent-iam component, which owns this prefix.
	case "agent-iam/tenant_baseline_policy_arn":
		c.TenantBaselinePolicyARN = value
	// ssm-producer: landing-zone's agent-iam component, which owns this prefix.
	case "agent-iam/tenant_permissions_boundary_arn":
		c.TenantPermissionsBoundaryARN = value
	// ssm-producer: landing-zone's agent-iam component, which owns this prefix.
	case "model-artifacts/bucket_name":
		c.ArtifactsBucketName = value
	case "bedrock/baseline_guardrail_id":
		c.BaselineGuardrailID = value
	case "bedrock/baseline_guardrail_version":
		c.BaselineGuardrailVersion = value
	case "kill-switch/event_bus_name":
		c.KillSwitchEventBusName = value
	case ampEndpointKey:
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
	}
}

// Validate returns a list of required-but-missing field names. The required set
// is the fields whose absence makes the operator do something other than what
// the substrate specified — in particular TenantPermissionsBoundaryARN: without
// it the operator mints tenant roles with no permissions boundary, silently.
// Optional integrations (guardrails, kill-switch, cost pipeline) are not in this
// set and degrade per-reconciler instead.
//
// The set itself is declared once, beside the SSM key that carries each field,
// and both this and MissingKeys read it. Two hand-kept copies of one set drift
// into disagreeing about what is required, and the disagreement is silent in
// whichever direction is smaller.
func (c *Config) Validate() []string {
	missing := []string{}
	for _, r := range requiredKeys {
		if r.Get(c) == "" {
			missing = append(missing, r.Field)
		}
	}
	return missing
}

// ampEndpointKey is the SSM key, under the cluster prefix, that
// managed-monitoring publishes the AMP workspace URL to. Named once because two
// readers use it: Load's sweep at startup, and AMPEndpoint's single read when
// the SLO reconciler re-resolves a client that was nil at boot.
const ampEndpointKey = "managed-monitoring/amp_endpoint"

// clusterPrefix is the SSM subtree this operator's substrate publishes under.
// Keyed by the full cluster name so co-located sibling clusters stay isolated.
func clusterPrefix(clusterName string) string {
	return "/eks-agent-platform/" + clusterName + "/"
}

// segment is what a single level of an SSM parameter path may contain. The
// cluster name becomes one level of clusterPrefix, so a name outside this makes
// a path that is not a path.
var segment = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// ValidPath refuses a cluster name that cannot appear in a parameter path.
//
// This is the one thing about the request that settles at startup, and it
// settles because the request is ours: the path is built here, from this name,
// so a name that cannot occupy a path level makes a call that is illegal now
// and illegal after any amount of waiting. Refusing it is not a guess about
// intent — it is a statement about a string this process constructed.
//
// It is deliberately narrow. It checks the shape of one path level and nothing
// else, so SSM may still refuse a path this accepts, for a rule not modelled
// here. Such a refusal arrives as an error from the sweep and is treated as an
// absence — waited on and reported — because from here it is indistinguishable
// from a substrate that has not been published yet.
func ValidPath(clusterName string) error {
	if clusterName == "" {
		return fmt.Errorf("operatorconfig: cluster name is required; it is the SSM subtree this operator reads")
	}
	if !segment.MatchString(clusterName) {
		return fmt.Errorf(
			"operatorconfig: cluster name %q cannot appear in an SSM parameter path, so %q is not a "+
				"path SSM can answer. A path level carries letters, digits and `_ . -`. This does not "+
				"become true later, so it is refused rather than waited on",
			clusterName, clusterPrefix(clusterName))
	}
	return nil
}

// AMPEndpoint reads just the managed-monitoring AMP endpoint.
//
// A parameter that does not exist yet is not an error: managed-monitoring is
// opt-in, and it is applied after the cluster that hosts this operator, so
// "absent" is both a normal steady state and the state a first install passes
// through. It answers "" in that case and the caller decides what that means.
//
// Separate from Load because the SLO reconciler calls this on a timer while its
// client is nil, and re-running the full sweep would re-read every parameter —
// including the ones whose absence Validate treats as fatal — to answer a
// question about one of them.
func AMPEndpoint(ctx context.Context, ssmClient awsclients.SSM, clusterName, environment, region string) (string, error) {
	if clusterName == "" {
		return "", fmt.Errorf("operatorconfig: cluster name is required")
	}
	name := clusterPrefix(clusterName) + ampEndpointKey
	out, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &name,
		WithDecryption: ptrBool(true),
	})
	if err != nil {
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return "", nil
		}
		return "", fmt.Errorf("ssm GetParameter %s: %w", name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", nil
	}
	return *out.Parameter.Value, nil
}

func ptrBool(b bool) *bool { return &b }
