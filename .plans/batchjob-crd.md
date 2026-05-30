# BatchJob CRD — Bedrock batch inference

Master plan: `/Users/bs/.claude/plans/prancy-rolling-dijkstra.md` Phase 3.

Absorbs claudium's Anthropic-Batch capability, re-shaped onto AWS Bedrock batch
(`CreateModelInvocationJob`) as a new `agents.stxkxs.io` CRD. No equivalent
existed in nanohype.

## Architecture: AWS-SDK-direct, not Argo

A Bedrock batch job runs server-side, so — unlike EvalSuite (which schedules an
in-cluster Argo Workflow) — there is nothing to run in a pod. The controller
manages the job directly via the AWS SDK (the BudgetPolicy pattern): submit
once, poll on a `RequeueAfter` tick to terminal, write output location + record
counts to status. **No gitops runtime addon** — the controller is the runtime,
shipped in `charts/operator`.

## Go operator

- `api/v1alpha1/batchjob_types.go` — Spec: `PlatformRef`, `ModelID`,
  `ModelInvocationType` (enum, default InvokeModel), `InputS3Uri`/`OutputS3Prefix`
  (`^s3://` pattern), `TimeoutHours` (24–168, default 24, Bedrock-imposed),
  `ServiceRoleArnOverride`. One CR = one job — no `Schedule` field (a dead no-op
  would be worse than its absence). Status: `JobArn` (idempotency guard),
  `JobName`, `Phase`, `SubmittedAt`/`CompletedAt`, `OutputLocation`,
  Record/Succeeded/Failed counts (from Bedrock's Processed/Success/Error
  counts), `Message`, `Conditions`.
- `internal/awsclients/bedrock.go` — `Bedrock` interface over
  `aws-sdk-go-v2/service/bedrock` (**control-plane**, not bedrockruntime) +
  wired into `Clients`/`New`.
- `internal/controller/batch_{controller,reconcile}.go` — finalizer dance +
  `RequeueAfter` polling (budget mirror); resolve PlatformRef → gate on Ready →
  submit (guarded by `status.JobArn` + a stable sha256 `clientRequestToken`,
  Bedrock's native idempotency, no DynamoDB) → poll `GetModelInvocationJob` →
  map status → phase. Finalizer best-effort-stops an in-flight job (never traps
  deletion; a leaked job is timeout-bounded). jobName sanitized to Bedrock's
  charset + fnv-hash-truncated to 63 chars.
- `cmd/main.go` — `--batch-workers` / `--batch-poll-interval` flags + reconciler
  wiring (`Bedrock` nil-gated like budget's Athena). `operatorconfig` +
  `PROJECT` extended.

## Two identities (the boundary)

- The **operator IRSA** (agent-iam operator role) gains `bedrock:*ModelInvocationJob`
  - `iam:PassRole` gated by `iam:PassedToService=bedrock.amazonaws.com`, scoped to
    the `eks-agent-platform/` role path — it _submits_ jobs.
- The **Bedrock batch service role** (new `terraform/components/batch-runtime`)
  is what Bedrock assumes to read input / write output — trusted by
  `bedrock.amazonaws.com` with `aws:SourceArn` (model-invocation-job), scoped to
  the shared model-artifacts bucket's `batch/` prefix + cmk-data via S3. ARN
  published to SSM (`batch-runtime/service_role_arn`), passed as the job's RoleArn.
  No new buckets — batch I/O stages under the existing artifacts bucket.

## Chart

`charts/operator`: `batchjobs`(+status,+finalizers) in the RBAC union;
`reconcilers.batch.{concurrent,pollInterval}` in values; `--batch-*` args in the
deployment. CRD copied by `make manifests`.

## Verify

```sh
cd operators
make generate    # deepcopy + config/crd/bases/agents.stxkxs.io_batchjobs.yaml + RBAC
make build       # compiles + go vet
make manifests   # copies CRD to charts/operator/crds + regenerates crd-reference
make lint        # golangci-lint: 0 issues
make test-conformance   # 5 BatchJob tests pass (round-trip + defaults + validation rejections + nil-Bedrock degrade)

cd ../terraform/components
tofu -chdir=batch-runtime init -backend=false && tofu -chdir=batch-runtime validate   # Success
tofu -chdir=agent-iam init -backend=false && tofu -chdir=agent-iam validate            # Success
tflint --chdir=batch-runtime ; tflint --chdir=agent-iam                                # clean
```
