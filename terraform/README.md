# terraform/

OpenTofu + Terragrunt for the platform's AWS-side substrate. Sits on top of [`landing-zone`](https://github.com/nanohype/landing-zone) (which provisions the EKS cluster, VPC, baseline IAM, and CMKs).

## Components

Two of these are **account-scoped** — applied once from `live/org/`, never per environment, because
what they own has exactly one instance per account. The rest are per-cluster.

| Component           | Scope   | Owns                                                                                                       |
| ------------------- | ------- | ------------------------------------------------------------------------------------------------------------ |
| `bedrock-account`   | account | Bedrock model-invocation logging: the account+region singleton, its Object Lock S3 destination, the CloudWatch log group and the delivery role. See [its README](components/bedrock-account/) before a first apply — the configuration is a blind put |
| `cost-pipeline`     | account | Athena workgroup + Glue database over landing-zone's CUR 2.0 export, the estimates bucket and table, the invocation-cost publisher, the spend-reconciliation view |
| `bedrock`           | cluster | baseline Guardrail (a guardrail is a named resource, so an account holds many)                              |
| `cost-access`       | cluster | this cluster's Athena workgroup + the operator's cost-read grant, and the SSM republish the operator reads |
| `agent-egress`      | cluster | VPC interface + gateway endpoints + optional WAF on the model gateway ALB                                  |
| `accelerator-pools` | cluster | Pod Identity roles for the NVIDIA GPU Operator + Neuron device plugin + pool catalog (SSM JSON)             |
| `kill-switch`       | cluster | EventBridge bus + Step Functions state machine for budget-breach detach                                    |
| `batch-runtime`     | cluster | Bedrock batch-inference service role, scoped to `model-invocation-job/*` and the `batch/` prefix of the model-artifacts bucket |
| `eval-runtime`      | cluster | Pod Identity role for the eval runner + its controller log group                                           |

There is no `model-artifacts` component in this tree. The buckets are owned by landing-zone's
`agent-iam`, which is their sole writer; components here read their ARNs from the per-cluster SSM
contract (`/eks-agent-platform/<cluster>/model-artifacts/*`) rather than from a sibling state.

## Dependency graph

Every component reads landing-zone's outputs — the cluster VPC / subnet / route-table / security-group IDs and CMK ARNs (as `TF_VAR_*`), and the operator role + tenant baseline from the `agent-iam` SSM contract (`/eks-agent-platform/<cluster>/agent-iam/*`, owned by landing-zone, not this tree). Intra-tree dependencies are minimal:

```
cost-pipeline → bedrock-account   (invocation log group)
cost-access   → cost-pipeline      (database, tables, results bucket, cost key, tenant IAM path)
eval-runtime  → agent-iam          (eval-reports bucket, landing-zone's contract)
batch-runtime → agent-iam          (model-artifacts bucket, landing-zone's contract)
```

The first two run account-first: `bedrock-account`, then `cost-pipeline`, then each cluster's
`cost-access`. Everything else (`bedrock`, `agent-egress`, `accelerator-pools`, `kill-switch`)
applies independently.

Every edge here is expressed through SSM rather than a terragrunt `dependency` block. That is
deliberate: terragrunt resolves `dependency` at **parse** time, so a per-cluster leaf pointing at an
account root would fail `init` — not `apply` — whenever that account state was absent, and no
`TF_VAR` gets you out of it.

## Apply order

Each environment is its own Terragrunt root. `terragrunt run --all apply` resolves the dependency graph above; `agent-iam` is applied separately as a landing-zone component (this tree only reads its SSM outputs).

## Wiring `landing-zone` outputs

Across all environments, the landing-zone-supplied infrastructure identifiers — KMS key ARNs, VPC/subnet IDs, route tables, the cluster security group, and the Karpenter node-role name — are passed in as `TF_VAR_*` environment variables by the orchestrator (the leaf `variables.tf` declares them; the `terragrunt.hcl` files don't pin them). For a manual run, `export` them alongside `AWS_ACCOUNT_ID`. Operator-side values — the operator role and tenant baseline — are read in-component from landing-zone's `agent-iam` SSM contract; the accelerator and eval-runner roles bind to their ServiceAccounts via EKS Pod Identity associations, so no OIDC issuer is consumed here. A future step may replace the `TF_VAR_*` handoff with `aws_ssm_parameter` data sources reading a stable `/landing-zone/<env>/*` output contract.

## Outputs

Per-cluster components publish their outputs to SSM under the **cluster name** (same key the
operator loads at startup), not the bare environment token:

```
/eks-agent-platform/<cluster>/<component>/<key>
```

The two account-scoped components publish under the `org` token instead, because there is no cluster
whose name they could honestly carry:

```
/eks-agent-platform/org/bedrock-account/invocation_log_group
/eks-agent-platform/org/cost-pipeline/<key>
```

The operator never reads that subtree. Its configuration is one recursive `GetParametersByPath`
sweep rooted at `/eks-agent-platform/<cluster>/`, so an `org/` parameter is invisible to it by
construction — which is why `cost-access` republishes the three account handles the operator needs
under the cluster prefix rather than the operator reaching across.

Consumers:

- **Operator pod** reads SSM at startup for `agent-iam.operator_role_arn` (its own role), `agent-iam.tenant_iam_path`, `agent-iam.tenant_baseline_policy_arn` (the `agent-iam.*` params are landing-zone's contract, not this tree's), `kill-switch.event_bus_name`, `cost-pipeline.athena_workgroup`, `cost-pipeline.athena_database`, `bedrock.baseline_guardrail_id`, `model-artifacts.bucket_name`.
- **accelerator roles** (`accelerator-pools.neuron_role_arn`, `accelerator-pools.gpu_operator_role_arn`) are bound to the device-plugin / operator ServiceAccounts by EKS Pod Identity associations created in this component — not by an annotation on the eks-gitops side.

## Backends

Each component writes to `s3://eks-agent-platform-tfstate-<account-id>-<region>` with native S3 locking (no DynamoDB table — `use_lockfile = true`). Create the bucket per environment before the first `apply`:

```bash
aws s3api create-bucket --bucket "eks-agent-platform-tfstate-${ACCOUNT_ID}-us-west-2" \
  --region us-west-2 --create-bucket-configuration LocationConstraint=us-west-2
aws s3api put-bucket-versioning --bucket "eks-agent-platform-tfstate-${ACCOUNT_ID}-us-west-2" \
  --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption --bucket "eks-agent-platform-tfstate-${ACCOUNT_ID}-us-west-2" \
  --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms"}}]}'
```

## Apply

```bash
task tofu:validate
task tofu:plan ENVIRONMENT=dev COMPONENT=bedrock
task tofu:apply ENVIRONMENT=dev COMPONENT=bedrock

# Or, run-all:
task tofu:apply ENVIRONMENT=dev COMPONENT=all
```
