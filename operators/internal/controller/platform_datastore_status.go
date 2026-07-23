/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// datastoreStatuses builds the observed-identity status for a Platform's
// declared datastores. Per the tenant-substrate boundary, the operator does not
// provision datastores — the tenant-substrate tofu module does — so this reports
// the stable identity a tenant uses to reach each store: the ARN, and, where the
// resource name is fully deterministic (S3, DynamoDB, SQS), the connection
// endpoint. Everything is composed from the same <env>-<platform>-<datastore>
// convention the module names by and the datastore-access policy scopes to, so
// the report needs no AWS call.
//
// The connection endpoints of Aurora, ElastiCache, and MSK carry an
// AWS-generated id, and the RDS-managed master-secret name is likewise generated
// (see SecretName below) — those are resolved out-of-band by the module outputs,
// not computed here, so Endpoint/SecretName are left empty for those kinds.
//
// Per-datastore Phase mirrors the Platform's own phase (identity + access
// readiness): a still-provisioning datastore does not gate the Platform's
// top-level Ready (that reflects namespace + identity), and its live AWS state
// is the module's concern rather than a phase the operator observes.
func datastoreStatuses(p *platformv1alpha1.Platform, env string, scope arnScope, phase string) []platformv1alpha1.DatastoreStatus {
	if len(p.Spec.Datastores) == 0 {
		return nil
	}

	part := scope.partition()
	region := scope.region()
	account := scope.account()
	tenant := p.Name

	out := make([]platformv1alpha1.DatastoreStatus, 0, len(p.Spec.Datastores))
	for _, d := range p.Spec.Datastores {
		base := fmt.Sprintf("%s-%s-%s", env, tenant, d.Name)
		st := platformv1alpha1.DatastoreStatus{Name: d.Name, Kind: d.Kind, Phase: phase}

		switch d.Kind {
		case platformv1alpha1.DatastoreObjectStore:
			bucket := fmt.Sprintf("%s-%s", base, account)
			st.ARN = fmt.Sprintf("arn:%s:s3:::%s", part, bucket)
			st.Endpoint = bucket
		case platformv1alpha1.DatastoreKeyValue:
			st.ARN = fmt.Sprintf("arn:%s:dynamodb:%s:%s:table/%s", part, region, account, base)
			st.Endpoint = base
		case platformv1alpha1.DatastoreQueue:
			name := base
			if d.Queue != nil && d.Queue.FIFO != nil && *d.Queue.FIFO {
				name += ".fifo"
			}
			st.ARN = fmt.Sprintf("arn:%s:sqs:%s:%s:%s", part, region, account, name)
			st.Endpoint = fmt.Sprintf("https://sqs.%s.amazonaws.com/%s/%s", region, account, name)
		case platformv1alpha1.DatastoreCache:
			st.ARN = fmt.Sprintf("arn:%s:elasticache:%s:%s:replicationgroup:%s", part, region, account, base)
		case platformv1alpha1.DatastoreStream:
			st.ARN = fmt.Sprintf("arn:%s:kafka:%s:%s:cluster/%s", part, region, account, base)
		case platformv1alpha1.DatastoreRelational:
			st.ARN = fmt.Sprintf("arn:%s:rds:%s:%s:cluster:%s", part, region, account, base)
		}

		out = append(out, st)
	}
	return out
}
