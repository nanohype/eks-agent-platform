# components/agent-iam

The **IRSA factory**. Provisions:

- The operator's own IRSA role with permissions to _manage_ tenant IAM resources (scoped strictly under `/eks-agent-platform/tenants/`), create KMS grants on `cmk-data`, mutate the artifacts bucket policy, and introspect Bedrock.
- A baseline `tenant_baseline` policy attached to every tenant role the operator stamps out. The baseline grants `bedrock:InvokeModel*` and `bedrock:ApplyGuardrail` constrained by `PrincipalTag` / `ResourceTag` equality on `PlatformId`, plus CloudWatch Logs writes to the per-tenant log path.

Per-tenant roles themselves are **not** created here — they are reconciled by the operator at `Platform` CR apply time.

## The trust model

```
EKS cluster OIDC issuer
        │  trusted by ──▶  operator role (this component)
        │                          │
        │                          │ iam:CreateRole / PutRolePolicy / AttachRolePolicy
        │                          │   scoped to: /eks-agent-platform/tenants/*
        │                          ▼
        │  trusted by ──▶  tenant-<platform-id> role (operator-managed)
        │                          │
        │                          │ bedrock:InvokeModel
        │                          │   scoped by tag equality
        │                          ▼
        │                  Bedrock models + tenant S3 prefix + KMS grant on cmk-data
```

The operator role is the _only_ thing in the system with broad IAM permissions. Tenants never have `iam:*`.

## Inputs

| Variable                                         | Description                            |
| ------------------------------------------------ | -------------------------------------- |
| `environment`                                    | dev / staging / production             |
| `region`, `cluster_name`                         | identifying                            |
| `oidc_provider_arn`, `oidc_issuer`               | from landing-zone cluster outputs      |
| `operator_namespace`, `operator_service_account` | defaults match the operator Helm chart |
| `tenant_iam_path`                                | default `/eks-agent-platform/tenants/` |
| `data_kms_key_arn`                               | `cmk-data` for grant creation          |
| `artifacts_bucket_arn`                           | for bucket-policy mutation             |

## Outputs

Under `/eks-agent-platform/<environment>/agent-iam/`:

- `operator_role_arn` — the operator Helm chart sets `serviceAccount.annotations.eks.amazonaws.com/role-arn` to this
- `tenant_iam_path` — operator config
- `tenant_baseline_policy_arn` — operator attaches this to every tenant role
