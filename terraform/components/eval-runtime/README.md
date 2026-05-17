# components/eval-runtime

AWS-side substrate for `EvalSuite` reconciliation. The Kubernetes-side
(Argo Workflows install + `eval-runner` `WorkflowTemplate` +
Argo Rollouts `AnalysisTemplate`) lives in `gitops/addons/eval-runtime/`
and is delivered via ArgoCD.

- **IRSA role** for the `eval-runner` ServiceAccount. Bedrock invoke
  (region-scoped via `aws:RequestedRegion`), S3 PutObject on the
  eval-reports bucket for HTML + junit artifacts, S3 GetObject scoped to
  `*/manifests/*` for `EvalSuite.spec.casesFromManifest`, KMS decrypt
  via `s3.<region>.amazonaws.com` for the SSE-KMS bucket.
- **Controller log group** (`/aws/eks/<cluster>/eval-runner`) with
  retention separate from per-Workflow pod logs so controller-level
  errors (template parse failures, scheduling) have their own retention
  policy.
- **SSM outputs** the operator picks up at startup:
  `/eks-agent-platform/<env>/eval-runtime/runner_role_arn` (and
  `runner_namespace`, `runner_service_account`, `eval_reports_bucket`).

The `EvalReconciler` emits an Argo `Workflow` (or `CronWorkflow` when
`spec.schedule` is set) into `runner_namespace` referencing the
`eval-runner` `WorkflowTemplate`. The template:

1. Pulls the cases (inline or from S3 manifest),
2. Invokes the agent under test via the Platform's `ModelGateway`,
3. Scores results against `expectContains` + `maxLatencyMs` +
   `maxCostUsd`,
4. Uploads HTML + junit to `eval-reports/<platform>/runs/<suite>/<runId>/`,
5. Writes `EvalSuite.status.lastScore` + `lastRunAt` back via the
   in-cluster API.

## Inputs

| Variable                                              | Description                                                      |
| ----------------------------------------------------- | ---------------------------------------------------------------- |
| `environment`, `region`, `cluster_name`               | identifying                                                      |
| `oidc_provider_arn`, `oidc_issuer`                    | EKS IRSA wiring (from landing-zone)                              |
| `eval_reports_bucket_arn`, `eval_reports_bucket_name` | from `model-artifacts`                                           |
| `bedrock_invoke_resource_arns`                        | default `["*"]`; production should pin to inference profile ARNs |
| `allowed_regions`                                     | `aws:RequestedRegion` ABAC for Bedrock                           |
| `data_kms_key_arn`, `logs_kms_key_arn`                | cmk-data + cmk-logs                                              |

## Outputs

- `eval_runner_role_arn` — annotated on the SA by `gitops/addons/eval-runtime`
- `eval_runner_namespace`, `eval_runner_service_account` — also published to SSM
- `controller_log_group_name`
