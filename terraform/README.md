# terraform/

OpenTofu + Terragrunt for the platform's AWS-side substrate. Sits on top of [`landing-zone`](https://github.com/stxkxs/landing-zone) (which provisions the EKS cluster, VPC, baseline IAM, and CMKs).

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
landing-zone (external) ──▶ cluster outputs: VPC, subnets, RTBs, cluster SG, OIDC issuer, CMK ARNs
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
                                  and gitops/ Helm values
```

## Apply order

Each environment is its own Terragrunt root. `terragrunt run-all apply` resolves the dependency graph above. Manual order if you prefer one-at-a-time:

```
model-artifacts → agent-iam → bedrock + agent-egress + accelerator-pools → kill-switch + cost-pipeline
```

## Wiring `landing-zone` outputs

Today the `terragrunt.hcl` files in `live/<env>/<component>/` reference landing-zone-supplied values as placeholder strings (`REPLACE_WITH_*`). For now, edit those by hand per environment. Once `landing-zone` publishes a stable per-environment output contract to SSM (`/landing-zone/<env>/cluster/*`), the `inputs` here will switch to `aws_ssm_parameter` data sources inside the components, removing the placeholder dance.

## Outputs

Every component publishes its outputs to SSM under:

```
/eks-agent-platform/<environment>/<component>/<key>
```

Consumers:

- **Operator pod** reads SSM at startup for `agent-iam.operator_role_arn` (its own role), `agent-iam.tenant_iam_path`, `agent-iam.tenant_baseline_policy_arn`, `kill-switch.event_bus_name`, `cost-pipeline.athena_workgroup`, `cost-pipeline.athena_database`, `bedrock.baseline_guardrail_id`, `accelerator-pools.pool_catalog`, `model-artifacts.bucket_name`.
- **GitOps Helm values** (`gitops/addons/*/values-<env>.yaml`) reference `accelerator-pools.neuron_role_arn` and `accelerator-pools.gpu_operator_role_arn` for IRSA annotations on the device plugin / operator ServiceAccounts.

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
