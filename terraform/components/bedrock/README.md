# components/bedrock

The per-cluster half of the Bedrock substrate: this cluster's baseline Guardrail, plus the
republish that puts the account's invocation-logging handles where the operator can find them.

- **Baseline Guardrail** — denied-topic filters at HIGH input + output, plus PII redaction
  (email, phone, credit-card → anonymize; SSN → block). A guardrail is a named resource, so an
  account holds many and each cluster gets its own. Tenants override or extend per route via
  `ModelGateway.spec.routes[].guardrailRef` (operator-reconciled); this baseline is the cluster's
  default.
- **Invocation-logging republish** — reads the account's log-group name and invocation-bucket ARN
  from the `bedrock-account` contract and republishes them under this cluster's SSM prefix.

The republish exists because of a shape mismatch, not for redundancy. Invocation logging is an
account+region singleton — `aws_bedrock_model_invocation_logging_configuration` has no name and no
identifier, so exactly one exists per account per region, and it is owned by
[`bedrock-account`](../bedrock-account/) under `/eks-agent-platform/org/`. The operator's entire
configuration is one recursive `GetParametersByPath` sweep of `/eks-agent-platform/<cluster>/`, so
a parameter published under the account prefix is invisible to it. Republishing keeps the
operator's contract unchanged while the values behind those keys come from the single component
that owns them.

Values arrive over SSM rather than a terragrunt `dependency`: a dependency across roots resolves
at config-parse time, so a per-cluster leaf would fail `init` — not `apply` — whenever the account
root's state was absent.

Per-tenant Bedrock access policies are **not** managed here — the operator creates them at
reconcile time, bound to each tenant's IAM role, with model-ARN scoping.

## Inputs

| Variable                     | Description                              |
| ---------------------------- | ---------------------------------------- |
| `cluster_name`               | EKS cluster name — used in the SSM paths |
| `enable_guardrails_baseline` | Toggle the baseline Guardrail            |
| `tags`                       | Common tags                              |

There is no Object Lock mode, retention window or teardown lever here. Those govern the invocation
record, which this component does not own — they live on
[`bedrock-account`](../bedrock-account/).

## Consumes

From the account contract:

- `/eks-agent-platform/org/bedrock-account/invocation_log_group`
- `/eks-agent-platform/org/bedrock-account/invocation_bucket_arn`

## Outputs

`baseline_guardrail_id`, `baseline_guardrail_version`.

Published to SSM under `/eks-agent-platform/<cluster_name>/bedrock/`:

- `invocation_bucket_arn`, `invocation_log_group` — republished from the account contract
- `baseline_guardrail_id`, `baseline_guardrail_version` — when the baseline is enabled

## Consumed by

- `kill-switch` reads `invocation_log_group` to subscribe a metric filter
- The operator reads `baseline_guardrail_id` and `baseline_guardrail_version` as the default when
  a `ModelGateway` route (and its `defaultGuardrailRef`) doesn't specify one. The version is
  load-bearing: an invocation pins a guardrail version, so publishing the id alone leaves the
  consumer unable to name what it is applying
- `cost-pipeline` subscribes its cost publisher to the same invocation log group, but reads the
  name from the account contract directly — it is account-scoped itself
