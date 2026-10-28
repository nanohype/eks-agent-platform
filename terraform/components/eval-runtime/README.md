# components/eval-runtime

AWS-side substrate for `EvalSuite` reconciliation. The Kubernetes side — the
`eval-runner` `WorkflowTemplate`, the Argo Rollouts `AnalysisTemplate` and the
RBAC an eval run holds — ships inside the operator chart at
`charts/operator/templates/eval-runtime/` (with manifests under
`charts/operator/files/eval-runtime/`), gated behind the chart's
`evalRuntime.*` values. Argo Workflows itself is installed by the eks-gitops
`addons-argo-platform` ApplicationSet, which satisfies the runtime's
precondition.

* **Controller log group** (`/aws/eks/<cluster>/eval-runner`) with
  retention separate from per-Workflow pod logs so controller-level
  errors (template parse failures, scheduling) have their own retention
  policy.
* **SSM outputs** the operator picks up at startup:
  `/eks-agent-platform/<cluster>/eval-runtime/` — `runner_namespace` and
  `eval_reports_bucket`. The namespace is where every eval `Workflow` is
  submitted and where each Platform's eval ServiceAccount is created; the
  bucket is the `eval-reports-bucket` argument every run uploads to and the
  bucket each Platform's eval role is scoped into.

## Identity: per Platform, minted by the operator

This component creates no IAM. An eval run holds its Platform's own identity,
which the operator reconciles alongside the rest of the Platform
(`operators/internal/controller/platform_eval_identity.go`):

| Object                   | Name                            | Where                                                            |
| ------------------------ | ------------------------------- | ---------------------------------------------------------------- |
| IAM role                 | `<cluster>-<platform>-eval`     | tenant IAM path, tenant permissions boundary, Pod Identity trust |
| ServiceAccount           | `eval-<platform>`               | `runner_namespace`                                               |
| Pod Identity association | that ServiceAccount → that role | the cluster                                                      |

The role's one inline policy, `eval-reports-access`, is generated from the
Platform name and the bucket:

- `s3:GetObject` on `<bucket>/<platform>/manifests/*` — the cases manifest
- `s3:PutObject` on `<bucket>/<platform>/runs/*` — `report.html` and `junit.xml`
- `kms:Decrypt` and `kms:GenerateDataKey`, only with `kms:ViaService` S3 in the
  operator's region and `kms:EncryptionContext:aws:s3:arn` equal to the bucket
  ARN — the context S3 uses for every key request on a bucket with an S3 Bucket
  Key

A run for one Platform reads no other Platform's cases and writes no other
Platform's reports; the bucket is shared, so that role is the only boundary
inside it. The role carries no Bedrock grant: eval-runner reaches the model
through the Platform's `ModelGateway` over HTTP.

The operator needs no grant on the role's root-path name beside its
tenant-path grants: it reads AccessDenied on the role's existence check as an
absent role and lets `CreateRole`, which names the tenant path, settle whether
the name is free. The Platform reports the identity on its `EvalIdentityReady`
condition, and no run is submitted until that is `True`.

The `EvalReconciler` emits an Argo `Workflow` (or `CronWorkflow` when
`spec.schedule` is set) into `runner_namespace` referencing the
`eval-runner` `WorkflowTemplate` and naming the Platform's eval
ServiceAccount. The template:

1. Pulls the cases (inline or from S3 manifest),
2. Invokes the agent under test via the Platform's `ModelGateway`,
3. Scores results against `expectContains` + `maxLatencyMs` +
   `maxCostUsd`,
4. Uploads HTML + junit to `eval-reports/<platform>/runs/<suite>/<runId>/`,
5. Writes `EvalSuite.status.lastScore` + `lastRunAt` back via the
   in-cluster API.

## Inputs

| Variable                | Description                                                                       |
| ----------------------- | --------------------------------------------------------------------------------- |
| `cluster_name`          | identifying; keys the SSM contract                                                |
| `eval_runner_namespace` | where eval Workflows run; must equal the operator chart's `evalRuntime.namespace` |
| `logs_kms_key_arn`      | the log-path key from landing-zone                                                |
| `log_retention_days`    | controller log retention                                                          |

The eval-reports bucket name is read from
`/eks-agent-platform/<cluster>/model-artifacts/eval_reports_bucket_name`, which
landing-zone's `agent-iam` publishes.

## Outputs

- `eval_runner_namespace` — published to SSM as `runner_namespace`
- `controller_log_group_name`
