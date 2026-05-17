# components/bedrock

Provisions the platform-wide Bedrock substrate:

- **Invocation logging** — S3 bucket (Object Lock, KMS-encrypted with `cmk-logs`, lifecycled to IA/Glacier) + CloudWatch Log Group + Bedrock `ModelInvocationLoggingConfiguration` pointed at both sinks. Bedrock writes the raw request + response + token usage for every invocation across the account.
- **Baseline Guardrail** — denied-topic filters at HIGH input + output, plus PII redaction (email, phone, credit-card → anonymize; SSN → block). Tenants override or extend per route via `GuardrailPolicy` CRs reconciled by the operator.

Per-tenant Bedrock access policies are **not** managed here — the operator creates them at reconcile time, bound to each tenant's IRSA role, with model-ARN scoping.

## Inputs

| Variable                     | Description                                   |
| ---------------------------- | --------------------------------------------- |
| `environment`                | dev / staging / production                    |
| `region`                     | AWS region                                    |
| `cluster_name`               | EKS cluster name (used in ARNs + SSM paths)   |
| `logs_kms_key_arn`           | `cmk-logs` from landing-zone                  |
| `log_retention_days`         | CloudWatch retention (default 365)            |
| `object_lock_mode`           | GOVERNANCE or COMPLIANCE (default GOVERNANCE) |
| `object_lock_retention_days` | Object-lock retention (default 365)           |
| `enable_guardrails_baseline` | Toggle the baseline Guardrail                 |
| `tags`                       | Common tags                                   |

## Outputs

Published to SSM under `/eks-agent-platform/<environment>/bedrock/`:

- `invocation_bucket_arn`, `invocation_bucket_name`
- `invocation_log_group_name`, `invocation_log_group_arn`
- `bedrock_logging_role_arn`
- `baseline_guardrail_id`, `baseline_guardrail_version`

## Consumed by

- `cost-pipeline` reads `invocation_log_group_name` for CloudWatch Metrics Insights queries
- `kill-switch` reads `invocation_log_group_arn` to subscribe a Metric Filter
- The operator reads `baseline_guardrail_id` as the default when a `GuardrailPolicy` CR doesn't specify one
