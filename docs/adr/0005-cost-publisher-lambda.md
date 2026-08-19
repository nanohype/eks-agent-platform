# ADR 0005 — In-flight Bedrock cost via Lambda republisher, not direct CloudWatch metric filter

## Status

Accepted (2026-05-16).

## Context

The `BudgetReconciler` needs to estimate Bedrock spend incurred since the last CUR partition (~24h lag). CloudWatch metric filters can pattern-match log events and emit metrics, which is the standard AWS pattern for this kind of thing. The Bedrock invocation log group is already populated by `terraform/components/bedrock`.

Three options for the in-flight cost signal:

1. **CloudWatch metric filter** on the invocation log group — pattern matches log lines, emits a metric. Native AWS, no Lambda.
2. **Subscription filter to a Lambda** that parses each log event, computes cost from token counts × per-model pricing, emits a `PutMetricData`. One Lambda invocation per log batch.
3. **No in-flight estimate** — rely on the CUR alone, accept the 24h breach-detection lag.

## Decision

Option 2. A Python 3.12 Lambda subscribes to the invocation log group via `aws_cloudwatch_log_subscription_filter`, parses each event for `{modelId, input.inputTokenCount, output.outputTokenCount, identity.arn}`, looks up per-1k token pricing for the model in a hardcoded table, sums by `PlatformId` (read from the IAM tags of the role named in `identity.arn`), and emits a single `PutMetricData` per batch with namespace `agents/Bedrock`, metric `EstimatedInvocationCostUsd`, dimension `PlatformId`.

## Why option 2 over option 1

CloudWatch metric filters are pattern-match only; they cannot do arithmetic. They can emit "the number of invocations matching this filter" or "the value of the matched field" but not "matched field × literal × another matched field". Bedrock cost requires `input_tokens × input_price + output_tokens × output_price`, with `input_price` and `output_price` varying by `modelId`. A metric filter cannot express this; we'd need:

- one metric filter per model variant (currently ~10) for input tokens,
- one metric filter per model variant for output tokens,
- a CloudWatch metric math expression per Platform per model combining the two,
- a separate metric per Platform.

The combinatorial explosion (10 models × N tenants × 2 metrics each) blows past CloudWatch's per-account metric count quotas quickly, and adding a model requires touching N+10 terraform resources. A Lambda with a `dict[str, dict[str, float]]` pricing table is cleaner.

## Why option 2 over option 3

A 24h breach-detection lag is the difference between "kill-switch fires when the breach happens" and "kill-switch fires the day after a runaway agent burned $5k". The in-flight estimate lets the kill-switch trip on the live signal; the CUR-only path means a deliberate or accidental spend spike runs unchecked for up to 24h.

Acceptable in dev where budgets are small ($1500/mo for the example tenants); unacceptable in production where a tenant could burn 7 figures in 24h on a misconfigured loop.

## Where the Lambda's pricing table comes from

The pricing table inside the Lambda is generated from `packages/pricing/src/data/bedrock-pricing.json` by `scripts/gen-lambda-pricing.mjs` — the same file the TypeScript `@eks-agent/pricing` package imports — with a CI drift gate (`gen-lambda-pricing.mjs --check`) holding the two in lockstep. Change a price in the JSON and regenerate; never hand-edit `pricing_data.py`. Two properties follow:

1. The metric is for alerting, not invoicing. CUR remains authoritative for the actual bill, and the estimate exists to close the ~24h partition lag rather than to reproduce it.
2. A model with no row in the table is not priced against a borrowed rate — that would silently undercount spend on a new or mistyped model id. It is published as an explicit `UnpricedInvocations` count dimensioned by PlatformId + ModelId, so unpriced traffic is observable and the table can be extended before the next billing cycle.

## Trade-offs

- **Pricing table drift.** AWS adjusts Bedrock pricing occasionally. The Lambda's table needs updates; otherwise the in-flight estimate diverges from CUR over time. Mitigated by quarterly review of the pricing table + Renovate-style PR automation as a future improvement.
- **Reserved concurrency at 25.** Limits the Lambda's blast radius (a runaway tenant generating 10k invocations/sec can't drain the account's Lambda quota), but also means burst invocation logs can queue behind reserved-concurrency-throttled invocations. Acceptable: the in-flight metric is a "running estimate", not a per-invocation log; lossy batching is fine.
- **An IAM read on the attribution path.** The Lambda resolves `PlatformId` by calling `iam:ListRoleTags` on the role named in `identity.arn`, rather than reconstructing the value from the role's name. That trades a name-parsing contract for a permission and a network call: the grant is load-bearing (without it every invocation attributes to `unknown`), and IAM's endpoint is global and throttled. Mitigated by memoizing per role for the life of the execution environment — a log batch is overwhelmingly the same few roles — and by not caching transient failures, so a throttle cannot pin a role to `unknown` for the container's life. The alternative was rejected because a derivation has no failure signal: the role name is assembled by the operator in Go and would be taken apart here in Python, and a disagreement between the two produces a metric published to a dimension nobody queries, with the Lambda running and nothing red.
- **`unknown` is a published dimension.** Spend the Lambda cannot attribute is emitted under `PlatformId="unknown"` and counted per role as `UnattributedInvocations`, rather than dropped. It reaches no Platform's budget either way; publishing it is what makes the gap observable instead of a quiet shortfall in someone else's number.

  No alarm on it ships from this component. Alerting is the observability layer's boundary (`eks-gitops`), not the cost pipeline's, and this repo creates no CloudWatch alarms at all — inventing an SNS topic and a notification target here to satisfy one metric would put the routing decision in the wrong layer. Until that alarm exists the metric is a signal that has to be looked at rather than one that arrives, and the gap is named here rather than implied to be covered.
- **Lambda failure mode.** A corrupt CloudWatch log batch (which AWS can produce during service events) is caught by the `_decode_payload` guard (returns an empty `CONTROL_MESSAGE` on decode failure) so the subscription doesn't retry-loop forever.

## Cross-references

- Implementation: `terraform/components/cost-pipeline/lambda/invocation_cost_publisher.py`.
- IAM scope: cloudwatch:PutMetricData conditioned on `namespace = agents/Bedrock`.
- Consumer: `operators/internal/controller/budget_reconcile.go` (`queryInflightCost`).
- Pricing table source: `packages/pricing/src/data/bedrock-pricing.json`, refreshed by `scripts/refresh-pricing.mjs` from the AWS Price List API (weekly); `scripts/gen-lambda-pricing.mjs --check` is the CI drift gate against the Lambda's copy.
- Flow diagram: [`docs/architecture/budget-reconcile-flow.md`](../architecture/budget-reconcile-flow.md).
