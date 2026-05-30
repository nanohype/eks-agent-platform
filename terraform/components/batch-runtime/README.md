# batch-runtime

The substrate for the `BatchJob` CRD — the Amazon Bedrock batch service role
the operator's BatchJob reconciler passes to `CreateModelInvocationJob`.

A Bedrock batch model-invocation job runs server-side: Bedrock reads the input
JSONL from S3, runs the model over each record, and writes the output JSONL
back. The job runs under a **service role** (distinct from the operator IRSA
that submits it). This component creates that role, scoped to the shared
`model-artifacts` bucket's `batch/` prefix where tenants stage batch I/O.

## What it creates

- An IAM role trusted by `bedrock.amazonaws.com`, scoped with
  `aws:SourceAccount` + `aws:SourceArn` (`model-invocation-job/*`) per the
  [AWS batch-inference service-role guidance](https://docs.aws.amazon.com/bedrock/latest/userguide/batch-iam-sr.html).
- A scoped data-access policy: `s3:GetObject`/`s3:PutObject` on
  `<artifacts-bucket>/batch/*`, `s3:ListBucket` on that prefix, and
  `kms:Decrypt`/`GenerateDataKey` on `cmk-data` via S3.
- An SSM parameter `/eks-agent-platform/<env>/batch-runtime/service_role_arn`
  the operator reads at startup (`operatorconfig.BatchServiceRoleARN`).

## Boundary

The operator (`agent-iam` operator role) holds the `bedrock:*ModelInvocationJob`
actions and `iam:PassRole` (gated by `iam:PassedToService=bedrock.amazonaws.com`)
to submit jobs and pass this role. No per-tenant fast-moving state lives here —
the role is account-level and slow-moving, so it sits in the substrate layer,
not in the operator's runtime reconciliation.
