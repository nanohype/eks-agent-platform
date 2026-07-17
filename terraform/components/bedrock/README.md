# components/bedrock

Provisions the platform-wide Bedrock substrate:

- **Invocation logging** — S3 bucket (Object Lock, KMS-encrypted with `cmk-logs`, lifecycled to IA/Glacier) + CloudWatch Log Group + Bedrock `ModelInvocationLoggingConfiguration` pointed at both sinks. Bedrock writes the raw request + response + token usage for every invocation across the account. Object Lock defaults to **GOVERNANCE** — logs are immutable by default, but an admin (`s3:BypassGovernanceRetention`) can clear the lock so the environment tears down cleanly. Set `object_lock_mode = "COMPLIANCE"` for a tenant that needs hard immutability; COMPLIANCE-locked objects (and the bucket) then can't be deleted by anyone until retention expires.
- **Baseline Guardrail** — denied-topic filters at HIGH input + output, plus PII redaction (email, phone, credit-card → anonymize; SSN → block). Tenants override or extend per route via `ModelGateway.spec.routes[].guardrailRef` (operator-reconciled); this baseline is the account-wide default.

Per-tenant Bedrock access policies are **not** managed here — the operator creates them at reconcile time, bound to each tenant's IRSA role, with model-ARN scoping.

## Inputs

| Variable                     | Description                                                                             |
| ---------------------------- | --------------------------------------------------------------------------------------- |
| `environment`                | dev / staging / production                                                              |
| `region`                     | AWS region                                                                              |
| `cluster_name`               | EKS cluster name (used in ARNs + SSM paths)                                             |
| `logs_kms_key_arn`           | `cmk-logs` from landing-zone                                                            |
| `log_retention_days`         | CloudWatch retention (default 365)                                                      |
| `object_lock_mode`           | GOVERNANCE (default; admin can bypass to delete) or COMPLIANCE (immutable until expiry) |
| `object_lock_retention_days` | Object-lock retention (default 365)                                                     |
| `enable_guardrails_baseline` | Toggle the baseline Guardrail                                                           |
| `tags`                       | Common tags                                                                             |

## Outputs

Published to SSM under `/eks-agent-platform/<environment>/bedrock/`:

- `invocation_bucket_arn`, `invocation_bucket_name`
- `invocation_log_group_name`, `invocation_log_group_arn`
- `bedrock_logging_role_arn`
- `baseline_guardrail_id`, `baseline_guardrail_version`

## Consumed by

- `cost-pipeline` reads `invocation_log_group_name` for CloudWatch Metrics Insights queries
- `kill-switch` reads `invocation_log_group_arn` to subscribe a Metric Filter
- The operator reads `baseline_guardrail_id` as the default when a `ModelGateway` route (and its `defaultGuardrailRef`) doesn't specify one
