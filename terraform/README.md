# terraform/

OpenTofu + Terragrunt for the platform's AWS-side substrate. Sits on top of [`landing-zone`](https://github.com/nanohype/landing-zone) (which provisions the EKS cluster, VPC, baseline IAM, and CMKs).

## Components

| Component           | Owns                                                                                            |
| ------------------- | ----------------------------------------------------------------------------------------------- |
| `bedrock`           | Invocation logging (S3 + Object Lock + CloudWatch Logs) + baseline Guardrail                    |
| `model-artifacts`   | KMS-encrypted S3 buckets for LoRA/adapter weights and eval reports                              |
| `agent-egress`      | VPC interface + gateway endpoints + optional WAF on the agentgateway ALB                        |
| `accelerator-pools` | Pod Identity roles for the NVIDIA GPU Operator + Neuron device plugin + pool catalog (SSM JSON) |
| `kill-switch`       | EventBridge bus + Step Functions state machine for budget-breach detach                         |
| `cost-pipeline`     | CUR 2.0 + Athena workgroup + Glue database + operator cost-read policy                          |

## Dependency graph

Every component reads landing-zone's outputs — the cluster VPC / subnet / route-table / security-group IDs and CMK ARNs (as `TF_VAR_*`), and the operator role + tenant baseline from the `agent-iam` SSM contract (`/eks-agent-platform/<env>/agent-iam/*`, owned by landing-zone, not this tree). Intra-tree dependencies are minimal:

```
eval-runtime → model-artifacts   (eval-reports bucket)
cost-pipeline → bedrock          (invocation log group)
```

Everything else (`model-artifacts`, `bedrock`, `agent-egress`, `accelerator-pools`, `kill-switch`, `batch-runtime`) applies independently. Each component writes its outputs to SSM (`/eks-agent-platform/<env>/*`), consumed by the operator in-cluster and by eks-gitops Helm values.

## Apply order

Each environment is its own Terragrunt root. `terragrunt run --all apply` resolves the dependency graph above; `agent-iam` is applied separately as a landing-zone component (this tree only reads its SSM outputs).

## Wiring `landing-zone` outputs

Across all environments, the landing-zone-supplied infrastructure identifiers — KMS key ARNs, VPC/subnet IDs, route tables, the cluster security group, and the Karpenter node-role name — are passed in as `TF_VAR_*` environment variables by the orchestrator (the leaf `variables.tf` declares them; the `terragrunt.hcl` files don't pin them). For a manual run, `export` them alongside `AWS_ACCOUNT_ID`. Operator-side values — the operator role and tenant baseline — are read in-component from landing-zone's `agent-iam` SSM contract; the accelerator and eval-runner roles bind to their ServiceAccounts via EKS Pod Identity associations, so no OIDC issuer is consumed here. A future step may replace the `TF_VAR_*` handoff with `aws_ssm_parameter` data sources reading a stable `/landing-zone/<env>/*` output contract.

## Outputs

Every component publishes its outputs to SSM under:

```
/eks-agent-platform/<environment>/<component>/<key>
```

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
