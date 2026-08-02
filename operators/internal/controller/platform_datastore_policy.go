/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// datastorePolicyName is the inline policy the operator reconciles onto a tenant
// role from spec.datastores. It grants the tenant exactly the actions its
// declared datastores need, scoped to the ARN patterns the tenant-substrate tofu
// module composes from <env>-<platform>-<datastore> (S3 account-qualified) — so
// the operator scopes access by naming convention without reading the module's
// outputs, and the per-app hand-typed action list ceases to exist. Anything the
// vocabulary does not cover rides spec.identity.extraPolicyArns, kept as a
// reviewed managed policy rather than a JSON blob in the spec.
const datastorePolicyName = "datastore-access"

// Per-kind action sets. Each is the minimum a tenant workload needs against its
// own store. Every Resource is scoped to the datastore's own name prefix, except
// the relational secret grant, whose ARN cannot be composed from the naming
// convention at all — see resolveRelationalSecretARNs.
var (
	datastoreDynamoActions = []string{
		"dynamodb:GetItem", "dynamodb:BatchGetItem", "dynamodb:Query", "dynamodb:Scan",
		"dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem",
		"dynamodb:BatchWriteItem", "dynamodb:ConditionCheckItem", "dynamodb:DescribeTable",
	}
	datastoreS3ObjectActions = []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"}
	datastoreS3BucketActions = []string{"s3:ListBucket", "s3:GetBucketLocation"}
	datastoreSQSActions      = []string{
		"sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage",
		"sqs:GetQueueAttributes", "sqs:GetQueueUrl", "sqs:ChangeMessageVisibility",
	}
	datastoreKafkaActions = []string{
		"kafka-cluster:Connect", "kafka-cluster:DescribeCluster", "kafka-cluster:DescribeTopic",
		"kafka-cluster:CreateTopic", "kafka-cluster:WriteData", "kafka-cluster:ReadData",
		"kafka-cluster:DescribeGroup", "kafka-cluster:AlterGroup",
	}
	datastoreSecretActions = []string{"secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"}
)

// sidToken sanitizes a datastore name into the [A-Za-z0-9] charset IAM Sids
// require. Datastore names are unique within a Platform, so the kind prefix plus
// the sanitized name yields a unique Sid per statement.
func sidToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// queueARNs returns the exact SQS ARNs a queue datastore can have, rather than a
// prefix. A prefix does not terminate: SQS queue names carry no delimiter, so
// `<env>-<platform>-<datastore>*` also matches every queue belonging to a Platform
// whose name begins with `<platform>-<datastore>`, and hyphenated Platform names are
// the house style. With sqs:ReceiveMessage and sqs:DeleteMessage in the action set
// and no queue policy anywhere to clamp it, that is one tenant draining another's
// queue. Every other datastore kind terminates on a delimiter — s3 `bucket/*`,
// dynamodb `table/index/*`, msk `cluster/<base>/*` — and this one now terminates by
// not wildcarding at all.
//
// There are at most two, mirroring tenant-substrate's queue.tf: the queue itself,
// and a dead-letter queue only when the datastore declares a redrive budget. FIFO is
// a single immutable bool, so `.fifo` is not an alternative spelling of the same
// queue — it is the only spelling that queue has.
func queueARNs(d platformv1alpha1.DatastoreSpec, base, part, region, account string) []string {
	suffix := ""
	if d.Queue != nil && d.Queue.FIFO != nil && *d.Queue.FIFO {
		suffix = ".fifo"
	}
	arns := []string{fmt.Sprintf("arn:%s:sqs:%s:%s:%s%s", part, region, account, base, suffix)}
	if d.Queue != nil && d.Queue.MaxReceiveCount > 0 {
		arns = append(arns, fmt.Sprintf("arn:%s:sqs:%s:%s:%s-dlq%s", part, region, account, base, suffix))
	}
	return arns
}

// datastorePolicyStatements builds the scoped statements for a Platform's
// declared datastores. tenant is the platform token composed into every name
// (spec.datastores resources are named <env>-<platform>-<datastore> by the
// tenant-substrate module, matching this). cache stores contribute no statement
// (ElastiCache data-plane access is network + auth-token, not IAM).
//
// secretARNs carries the resolved RDS-managed master-secret ARN per relational
// datastore name, looked up from SSM by resolveRelationalSecretARNs. A relational
// datastore with no resolved ARN contributes no grant: RDS names that secret from
// the Aurora cluster's own AWS-generated resource id, so there is no convention to
// fall back on, and the only pattern that would match without the real ARN is the
// account-wide rds!cluster-* prefix — which is every other tenant's master
// credentials. No grant is the correct failure mode; the caller surfaces the
// unresolved datastore and requeues.
func datastorePolicyStatements(p *platformv1alpha1.Platform, env string, scope arnScope, secretARNs map[string]string) []policyStatement {
	part := scope.partition()
	region := scope.region()
	account := scope.account()
	tenant := p.Name

	stmts := make([]policyStatement, 0, len(p.Spec.Datastores))
	var resolvedSecrets []string

	for _, d := range p.Spec.Datastores {
		base := fmt.Sprintf("%s-%s-%s", env, tenant, d.Name)
		tok := sidToken(d.Name)

		switch d.Kind {
		case platformv1alpha1.DatastoreObjectStore:
			bucket := fmt.Sprintf("arn:%s:s3:::%s-%s", part, base, account)
			stmts = append(stmts,
				policyStatement{
					Sid: "s3bucket" + tok, Effect: "Allow",
					Action: datastoreS3BucketActions, Resource: []string{bucket},
				},
				policyStatement{
					Sid: "s3object" + tok, Effect: "Allow",
					Action: datastoreS3ObjectActions, Resource: []string{bucket + "/*"},
				},
			)
		case platformv1alpha1.DatastoreKeyValue:
			table := fmt.Sprintf("arn:%s:dynamodb:%s:%s:table/%s", part, region, account, base)
			stmts = append(stmts, policyStatement{
				Sid: "dynamodb" + tok, Effect: "Allow",
				Action: datastoreDynamoActions, Resource: []string{table, table + "/index/*"},
			})
		case platformv1alpha1.DatastoreQueue:
			stmts = append(stmts, policyStatement{
				Sid: "sqs" + tok, Effect: "Allow",
				Action: datastoreSQSActions, Resource: queueARNs(d, base, part, region, account),
			})
		case platformv1alpha1.DatastoreStream:
			stmts = append(stmts, policyStatement{
				Sid: "msk" + tok, Effect: "Allow",
				Action: datastoreKafkaActions,
				Resource: []string{
					fmt.Sprintf("arn:%s:kafka:%s:%s:cluster/%s/*", part, region, account, base),
					fmt.Sprintf("arn:%s:kafka:%s:%s:topic/%s/*", part, region, account, base),
					fmt.Sprintf("arn:%s:kafka:%s:%s:group/%s/*", part, region, account, base),
				},
			})
		case platformv1alpha1.DatastoreRelational:
			if arn := secretARNs[d.Name]; arn != "" {
				resolvedSecrets = append(resolvedSecrets, arn)
			}
		case platformv1alpha1.DatastoreCache:
			// no IAM statement — access is network + auth token
		}
	}

	if len(resolvedSecrets) > 0 {
		// Sorted so an unchanged declaration marshals to an unchanged document
		// and ensureDatastorePolicy's drift comparison stays stable.
		sort.Strings(resolvedSecrets)
		stmts = append(stmts, policyStatement{
			Sid: "relationalSecrets", Effect: "Allow",
			Action:   datastoreSecretActions,
			Resource: resolvedSecrets,
		})
	}

	return stmts
}

// datastorePolicyDoc marshals the statements into an IAM policy document, or
// returns the empty string when there are none (the caller removes the inline
// policy in that case).
func datastorePolicyDoc(stmts []policyStatement) (string, error) {
	if len(stmts) == 0 {
		return "", nil
	}
	b, err := json.Marshal(policyDocument{Version: "2012-10-17", Statement: stmts})
	if err != nil {
		return "", fmt.Errorf("marshal datastore policy: %w", err) //coverage:ignore json.Marshal of a policyDocument of string fields cannot fail
	}
	return string(b), nil
}

// masterSecretParamPath is where the tenant-substrate component publishes a
// relational datastore's RDS-managed master-secret ARN. Keyed on the full cluster
// name — the same /eks-agent-platform/<cluster>/ subtree the operator resolves the
// rest of its configuration from, so co-located sibling clusters stay isolated.
func masterSecretParamPath(clusterName, platform, datastore string) string {
	return fmt.Sprintf("/eks-agent-platform/%s/tenant-substrate/%s/%s/master_secret_arn",
		clusterName, platform, datastore)
}

// resolveRelationalSecretARNs reads the published master-secret ARN for every
// relational datastore the Platform declares.
//
// A missing parameter is not an error. The substrate applies independently of the
// CR, so a Platform can legitimately reconcile before its Aurora cluster exists;
// that datastore comes back in the unresolved list, contributes no grant, and the
// caller reports it. The alternative — granting on the rds!cluster-* prefix until
// the real ARN shows up — is what made this cross-tenant in the first place.
func (r *PlatformReconciler) resolveRelationalSecretARNs(
	ctx context.Context, p *platformv1alpha1.Platform, clusterName string,
) (map[string]string, []string, error) {
	resolved := map[string]string{}
	var unresolved []string

	for _, d := range p.Spec.Datastores {
		if d.Kind != platformv1alpha1.DatastoreRelational {
			continue
		}
		// No SSM client or no cluster name means the lookup cannot be made at
		// all — unresolved, never a fallback to a broader pattern.
		if r.SSM == nil || clusterName == "" {
			unresolved = append(unresolved, d.Name)
			continue
		}

		value, err := r.readSSMParameter(ctx, masterSecretParamPath(clusterName, p.Name, d.Name))
		if err != nil {
			return nil, nil, err
		}
		if value == "" {
			unresolved = append(unresolved, d.Name)
			continue
		}
		resolved[d.Name] = value
	}

	return resolved, unresolved, nil
}

// readSSMParameter returns a parameter's value, or the empty string when it does
// not exist or carries no value. Every consumer of the tenant-substrate SSM
// contract treats "absent" the same way — no grant rather than a broader one —
// so the not-found case is normalized here instead of at each call site. A real
// failure (throttled, denied) still propagates: mistaking it for "not published"
// would quietly strip a working tenant's grant on the next converge.
func (r *PlatformReconciler) readSSMParameter(ctx context.Context, path string) (string, error) {
	out, err := r.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(path)})
	var notFound *ssmtypes.ParameterNotFound
	switch {
	case errors.As(err, &notFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("ssm GetParameter %s: %w", path, err)
	case out == nil || out.Parameter == nil:
		return "", nil
	default:
		return aws.ToString(out.Parameter.Value), nil
	}
}

// ensureDatastorePolicy reconciles the datastore-access inline policy on a tenant
// role. When the Platform declares no datastores needing IAM, the policy is
// removed so a cleared declaration leaves no stale grant. Idempotent: reads the
// current document and writes only on drift. Callers MUST NOT invoke this on a
// suspended role — ensureIamRole's suspension short-circuit returns first,
// keeping the operator observe-only under the kill-switch.
func (r *PlatformReconciler) ensureDatastorePolicy(ctx context.Context, roleName, roleARN string, p *platformv1alpha1.Platform, cfg IAMConfig) error {
	if r.IAM == nil {
		return nil
	}

	secretARNs, unresolved, err := r.resolveRelationalSecretARNs(ctx, p, cfg.ClusterName)
	if err != nil {
		return err
	}
	if len(unresolved) > 0 {
		// Visible rather than silent: the tenant's database credential is
		// unreachable until the substrate publishes, and an operator debugging a
		// pod that cannot read its own secret needs to land here, not on an
		// unexplained AccessDenied.
		log.FromContext(ctx).Info(
			"relational datastore has no published master-secret ARN yet; granting no Secrets Manager access for it",
			"platform", p.Name, "datastores", unresolved,
			"expectedParameter", masterSecretParamPath(cfg.ClusterName, p.Name, unresolved[0]),
		)
	}

	stmts := datastorePolicyStatements(p, cfg.Environment, arnScopeFromRole(roleARN, cfg.Region), secretARNs)
	desired, err := datastorePolicyDoc(stmts)
	if err != nil {
		return err //coverage:ignore only reachable if json.Marshal fails, which it cannot for this document
	}

	if desired == "" {
		// No datastore needs IAM — ensure the inline policy is absent.
		if _, err := r.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName:   aws.String(roleName),
			PolicyName: aws.String(datastorePolicyName),
		}); err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("iam DeleteRolePolicy %s/%s: %w", roleName, datastorePolicyName, err)
		}
		return nil
	}

	getOut, getErr := r.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
		RoleName:   aws.String(roleName),
		PolicyName: aws.String(datastorePolicyName),
	})
	if getErr == nil && getOut != nil && policyDocEqual(aws.ToString(getOut.PolicyDocument), desired) {
		return nil
	}
	if getErr != nil && !isIAMNotFound(getErr) {
		return fmt.Errorf("iam GetRolePolicy %s/%s: %w", roleName, datastorePolicyName, getErr)
	}

	if _, err := r.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(datastorePolicyName),
		PolicyDocument: aws.String(desired),
	}); err != nil {
		return fmt.Errorf("iam PutRolePolicy %s/%s: %w", roleName, datastorePolicyName, err)
	}
	return nil
}
