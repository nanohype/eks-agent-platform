# Cost estimate-vs-CUR reconciliation + dashboards

Master plan: `/Users/bs/.claude/plans/prancy-rolling-dijkstra.md` Phase 2.

Absorbs claudium's CUR×call-event reconciliation idea into the existing
`cost-pipeline` component (where both the CUR table and the EMF cost estimate
already live). All changes are in `terraform/components/cost-pipeline/` + the
finance dashboard.

## Latent bug fixed (blocking)

`lambda/invocation_cost_publisher.py` reads `AGENTS_ENVIRONMENT` to strip the
`<env>-` prefix from the role-derived PlatformId, but the `aws_lambda_function`
had **no `environment{}` block** — so the metric dimension stayed `<env>-acme`
while CUR and the Budget reconciler use the bare `acme`
(`budget_reconcile.go:113,217`). In-flight cost therefore read zero. Added the
env block (`AGENTS_ENVIRONMENT = var.environment`), which repairs the existing
budget path and is the precondition for the reconciliation join.

## Estimate export → Athena

- `invocation_cost_publisher.py`: aggregates per-(platform, model)
  cost/in/out/count; `_emit_metrics` unchanged (cost by PlatformId — the Budget
  reconciler's input); adds `TokensIn`/`TokensOut` metrics (PlatformId+ModelId)
  and `_write_estimates()` → one NDJSON object per batch under
  `<cur-bucket>/estimates/usage_date=<d>/`. S3 write is wrapped so a hiccup
  never poisons the metric path. Stdlib + boto3 only.
- `main.tf`: `aws_glue_catalog_table.estimates` over that prefix. **Partition
  projection on `usage_date` only** (date type); `platform_id` is a data
  column, not a partition — claudium's design used an `injected` platform
  partition, which would reject the aggregate-all reconciliation query (injected
  projection demands a per-partition predicate). Lambda role gains
  `s3:PutObject` (estimates/\*) + `kms:GenerateDataKey` (ViaService s3). New
  lifecycle rule expires the `estimates/` prefix after `estimate_retention_days`
  (default 90) — CUR Parquet under `cur/` untouched.

## Reconciliation view

`aws_athena_named_query.spend_reconciliation` holds a plain `CREATE OR REPLACE
VIEW invocation_cost_reconciliation AS …` (daily estimate SUM LEFT JOIN CUR
`AmazonBedrock` truth on platform_id + day → `delta_usd`/`delta_pct`). Chose a
saved query over a Glue `VIRTUAL_VIEW` because the latter needs a base64 Presto
envelope whose column types must match exactly and can't be validated without a
live Athena — plain reviewable SQL is the lower-risk "right the first time"
choice. **One-time materialization:** run the saved query once in the cost
workgroup to create the view (the finance dashboard reads it by name). Name +
estimate table published to SSM.

## Dashboard (`gitops/dashboards/finance.json`)

- Fixed line 30's CUR-2.0 map-column syntax → flat `resource_tags_user_platformid`.
- Added: estimate-vs-truth overlay (timeseries), estimate-drift table
  (`delta_pct`), tokens-in/out by model (cloudwatch). The reconciliation panels
  query the view by name, so the dashboard never hard-codes the CUR table name.

## Metric classification (built only what has a real source)

- **Built:** estimate-vs-truth, drift, tokens-in/out (Lambda already parses tokens).
- **Reused (no new infra):** latency p50/95/99 + model-errors — agentgateway
  already exports `agentgateway_invocation_duration_seconds_bucket` /
  `agentgateway_invocation_total` to Prometheus.
- **Deferred (no source):** cache-hit % (cost pipeline sees invocation logs, not
  response-body cache tokens) and audit lag (no audit-delivery pipeline). Not built.

## Verify

```sh
python3 -m py_compile terraform/components/cost-pipeline/lambda/invocation_cost_publisher.py
tofu -chdir=terraform/components/cost-pipeline fmt -check
tofu -chdir=terraform/components/cost-pipeline init -backend=false && tofu -chdir=terraform/components/cost-pipeline validate   # Success
tflint --chdir=terraform/components/cost-pipeline                                                                              # clean
jq -e '.panels | length' gitops/dashboards/finance.json
```

Acceptance after deploy: an `estimates/usage_date=…` object exists carrying the
**bare** `platform_id`; the materialized view returns non-NULL `cur_truth_usd`
for a platform with both estimate + CUR data (proves the PlatformId keys agree).
