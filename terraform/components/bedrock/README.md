# components/bedrock

The per-cluster half of the Bedrock substrate: this cluster's baseline Guardrail.

A guardrail is a named resource, so an account holds many and each cluster gets its own. That is
the whole reason anything here is per-cluster — denied-topic filters at HIGH input + output, plus
PII redaction (email, phone, credit-card → anonymize; SSN → block). Tenants override or extend per
route via `ModelGateway.spec.routes[].guardrailRef` (operator-reconciled); this baseline is the
cluster's default.

Guardrails are not offered in every region, so the baseline is toggleable. When it is off — by the
toggle or because the region does not support it — neither key is published at all rather than
published empty: a key carrying a blank id reads to the operator as a configured guardrail and
would be applied in place of the route's own reference.

Invocation logging is **not** here. Its configuration, bucket and log group are account+region
singletons owned by [`bedrock-account`](../bedrock-account/) — the AWS resource has no name and no
identifier, so exactly one exists per account per region — and consumers read them from the account
contract at `/eks-agent-platform/org/bedrock-account/`. See [Invocation logging is not
here](#invocation-logging-is-not-here) below.

Per-tenant Bedrock access policies are **not** managed here either — the operator creates them at
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

## Outputs

`baseline_guardrail_id`, `baseline_guardrail_version`.

Published to SSM under `/eks-agent-platform/<cluster_name>/bedrock/`:

- `baseline_guardrail_id`, `baseline_guardrail_version` — when the baseline is enabled

A guardrail is a named resource, so an account holds many and each cluster gets its own.
That is the whole reason anything here is per-cluster.

## Consumed by

- The operator reads `baseline_guardrail_id` and `baseline_guardrail_version` as the default when
  a `ModelGateway` route (and its `defaultGuardrailRef`) doesn't specify one. The version is
  load-bearing: an invocation pins a guardrail version, so publishing the id alone leaves the
  consumer unable to name what it is applying

## Invocation logging is not here

The invocation-logging configuration, its bucket and its log group are account+region
singletons owned by [`bedrock-account`](../bedrock-account/), published at
`/eks-agent-platform/org/bedrock-account/`. Consumers read them there:

- `cost-pipeline` subscribes its cost publisher to the invocation log group. It is
  account-scoped itself, so it reads the account contract directly.

The operator is not a consumer. It has no CloudWatch Logs client and never addresses the
invocation bucket — the spend it acts on arrives as a CloudWatch metric the cost publisher
emits, and as CUR rows it reads through Athena.
