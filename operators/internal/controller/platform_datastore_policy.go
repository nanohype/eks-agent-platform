/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"

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
// own store; none is wildcarded, and every Resource is scoped to the datastore's
// own name prefix.
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

// datastorePolicyStatements builds the scoped statements for a Platform's
// declared datastores. tenant is the platform token composed into every name
// (spec.datastores resources are named <env>-<platform>-<datastore> by the
// tenant-substrate module, matching this). cache stores contribute no statement
// (ElastiCache data-plane access is network + auth-token, not IAM); relational
// stores share one secret grant because the RDS-managed master secret has an
// AWS-generated name and is scoped by the rds!cluster- prefix.
func datastorePolicyStatements(p *platformv1alpha1.Platform, env string, scope arnScope) []policyStatement {
	part := scope.partition()
	region := scope.region()
	account := scope.account()
	tenant := p.Name

	stmts := make([]policyStatement, 0, len(p.Spec.Datastores))
	needSecret := false

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
			// <base>, <base>.fifo, <base>-dlq, <base>-dlq.fifo all share the prefix.
			queue := fmt.Sprintf("arn:%s:sqs:%s:%s:%s*", part, region, account, base)
			stmts = append(stmts, policyStatement{
				Sid: "sqs" + tok, Effect: "Allow",
				Action: datastoreSQSActions, Resource: []string{queue},
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
			needSecret = true
		case platformv1alpha1.DatastoreCache:
			// no IAM statement — access is network + auth token
		}
	}

	if needSecret {
		stmts = append(stmts, policyStatement{
			Sid: "relationalSecrets", Effect: "Allow",
			Action:   datastoreSecretActions,
			Resource: []string{fmt.Sprintf("arn:%s:secretsmanager:%s:%s:secret:rds!cluster-*", part, region, account)},
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

	stmts := datastorePolicyStatements(p, cfg.Environment, arnScopeFromRole(roleARN, cfg.Region))
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
