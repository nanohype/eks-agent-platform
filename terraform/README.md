# terraform/

OpenTofu + Terragrunt for the platform's AWS-side substrate. Sits on top of [`landing-zone`](https://github.com/nanohype/landing-zone) (which provisions the EKS cluster, VPC, baseline IAM, and CMKs).

## Components

| Component           | Owns                                                                                |
| ------------------- | ----------------------------------------------------------------------------------- |
| `bedrock`           | Invocation logging (S3 + Object Lock + CloudWatch Logs) + baseline Guardrail        |
| `model-artifacts`   | KMS-encrypted S3 buckets for LoRA/adapter weights and eval reports                  |
| `agent-iam`         | Operator IRSA role + tenant-baseline policy + tenant IAM path                       |
| `agent-egress`      | VPC interface + gateway endpoints + optional WAF on the agentgateway ALB            |
| `accelerator-pools` | IRSA roles for NVIDIA GPU Operator + Neuron device plugin + pool catalog (SSM JSON) |
| `kill-switch`       | EventBridge bus + Step Functions state machine for budget-breach detach             |
| `cost-pipeline`     | CUR 2.0 + Athena workgroup + Glue database + operator cost-read policy              |

## Dependency graph

```
landing-zone (external) ──▶ cluster outputs: VPC, subnets, RTBs, cluster SG, CMK ARNs
                                                   │
                                                   ▼
                            ┌──────────────────────────────────────────────┐
                            │                                              │
                            ▼                                              ▼
                     model-artifacts                                 agent-egress
                            │                                              │
                            ▼                                              │
                       agent-iam ◀──────────────────────────┐              │
                       │   │   │                            │              │
            ┌──────────┘   │   └──────────┐                 │              │
            ▼              ▼              ▼                 │              │
        bedrock      kill-switch    cost-pipeline    accelerator-pools     │
            │              │              │                 │              │
            └──────────────┴──────────────┴─────────────────┴──────────────┘
                                          │
                                          ▼
                                  SSM Parameter Store
                                  /eks-agent-platform/<env>/*
                                          │
                                          ▼
                            consumed by operator (in-cluster)
                                  and eks-gitops Helm values
```

## Apply order

Each environment is its own Terragrunt root. `terragrunt run-all apply` resolves the dependency graph above. Manual order if you prefer one-at-a-time:

```
model-artifacts → agent-iam → bedrock + agent-egress + accelerator-pools → kill-switch + cost-pipeline
```

## Wiring `landing-zone` outputs

Across all environments, the landing-zone-supplied infrastructure identifiers — KMS key ARNs, VPC/subnet IDs, route tables, the cluster security group, and the Karpenter node-role name — are passed in as `TF_VAR_*` environment variables by the orchestrator (the leaf `variables.tf` declares them; the `terragrunt.hcl` files don't pin them). For a manual run, `export` them alongside `AWS_ACCOUNT_ID`. Operator-side values — the operator role and tenant baseline — are read in-component from landing-zone's `agent-iam` SSM contract; the accelerator and eval-runner roles bind to their ServiceAccounts via EKS Pod Identity associations, so no OIDC issuer is consumed here. A future step may replace the `TF_VAR_*` handoff with `aws_ssm_parameter` data sources reading a stable `/landing-zone/<env>/*` output contract.

## Outputs

Every component publishes its outputs to SSM under:

```
/eks-agent-platform/<environment>/<component>/<key>
```

Consumers:

- **Operator pod** reads SSM at startup for `agent-iam.operator_role_arn` (its own role), `agent-iam.tenant_iam_path`, `agent-iam.tenant_baseline_policy_arn`, `kill-switch.event_bus_name`, `cost-pipeline.athena_workgroup`, `cost-pipeline.athena_database`, `bedrock.baseline_guardrail_id`, `accelerator-pools.pool_catalog`, `model-artifacts.bucket_name`.
- **eks-gitops accelerator values** (`eks-gitops/addons/accelerators/<addon>/values-<env>.yaml`) reference `accelerator-pools.neuron_role_arn` and `accelerator-pools.gpu_operator_role_arn` for IRSA annotations on the device plugin / operator ServiceAccounts.

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
