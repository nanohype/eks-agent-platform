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

Option 2. A Python 3.12 Lambda subscribes to the invocation log group via `aws_cloudwatch_log_subscription_filter`, parses each event for `{modelId, input.inputTokenCount, output.outputTokenCount, identity.arn}`, looks up per-1k token pricing for the model in a hardcoded table, sums by `PlatformId` (extracted from the assumed-role ARN's session name), and emits a single `PutMetricData` per batch with namespace `agents/Bedrock`, metric `EstimatedInvocationCostUsd`, dimension `PlatformId`.

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

## Why "conservative-rounded" pricing in the Lambda

The pricing table inside the Lambda is hand-maintained from public Bedrock pricing pages, rounded _upward_ (the comment in `invocation_cost_publisher.py` calls this out). Two reasons:

1. The metric is for alerting, not invoicing. CUR remains authoritative for the actual bill. An estimate that's slightly high trips alerts slightly early — desired behavior.
2. Pricing changes (AWS price reductions, new models added with default pricing unknown to the Lambda) get a sensible fallback (`FALLBACK_PRICING` is mid-range Sonnet pricing) so unknown models alert at the conservative end.

## Trade-offs

- **Pricing table drift.** AWS adjusts Bedrock pricing occasionally. The Lambda's table needs updates; otherwise the in-flight estimate diverges from CUR over time. Mitigated by quarterly review of the pricing table + Renovate-style PR automation as a future improvement.
- **Reserved concurrency at 25.** Limits the Lambda's blast radius (a runaway tenant generating 10k invocations/sec can't drain the account's Lambda quota), but also means burst invocation logs can queue behind reserved-concurrency-throttled invocations. Acceptable: the in-flight metric is a "running estimate", not a per-invocation log; lossy batching is fine.
- **PlatformId extraction from assumed-role ARN.** Couples the Lambda to the tenant role naming pattern documented in ADR 0003. If that pattern changes, the Lambda silently extracts wrong PlatformIds. Mitigation: the `agents.stxkxs.io/env` Lambda env var anchors the prefix, and the role name pattern is the same canonical contract the kill-switch SFN reads (so they fail together if the pattern changes, which is the right blast radius).
- **Lambda failure mode.** A corrupt CloudWatch log batch (which AWS can produce during service events) is caught by the `_decode_payload` guard (returns an empty `CONTROL_MESSAGE` on decode failure) so the subscription doesn't retry-loop forever.

## Cross-references

- Implementation: `terraform/components/cost-pipeline/lambda/invocation_cost_publisher.py`.
- IAM scope: cloudwatch:PutMetricData conditioned on `namespace = agents/Bedrock`.
- Consumer: `operators/internal/controller/budget_reconcile.go` (`queryInflightCost`).
- Pricing table source: AWS Bedrock pricing page; manual quarterly review.
- Flow diagram: [`docs/architecture/budget-reconcile-flow.md`](../architecture/budget-reconcile-flow.md).
