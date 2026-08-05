# components/cost-pipeline

The account's spend query layer — what `BudgetPolicy` reconciliation reads to decide whether a
tenant is over its cap.

Applied **once per account**, from `live/org/`. A Cost and Usage Report always
covers the whole account, so a per-environment copy of this component would be a complete duplicate
of the same billing data rather than a view of one environment's share. Everything that is genuinely
per-cluster — the operator's IAM grant, that cluster's Athena workgroup, the republished handles
under the cluster prefix the operator sweeps — lives in [`cost-access`](../cost-access/).

## Pieces

- **The CUR table** — a Glue table declared over landing-zone's CUR 2.0 export, at the export's
  `data/` prefix, with `BILLING_PERIOD` partitions projected rather than registered. Its name is
  this component's own (`cur`), published to SSM, and the operator queries exactly that. Nothing
  discovers it and nothing predicts it. See the block comment in `main.tf` for why there is no
  crawler and why the partitions are projected.
- **Glue database + the account's Athena workgroup** — the account's query surface, where the
  reconciliation named query binds and where an analyst runs. Results are encrypted with the platform data CMK
  and expire on `athena_results_retention_days`. This is *not* the workgroup any cluster's budget
  reconciler uses; each cluster gets its own from `cost-access`, writing to its own results prefix.
- **Invocation-cost-publisher Lambda** — subscribes to the Bedrock invocation log group owned by
  [`bedrock-account`](../bedrock-account/) and republishes per-invocation cost as the CloudWatch
  metric `agents/Bedrock:EstimatedInvocationCostUsd`, dimensioned by `PlatformId`. The Budget
  reconciler reads this for in-flight cost: invocation logs land in seconds, CUR rows lag by ~24h.
  The Lambda's pricing table is rough and rounds conservatively upward; CUR stays authoritative.
- **Estimates table + reconciliation view** — the publisher also writes per-(platform, model) daily
  estimate records as Hive-partitioned NDJSON. `invocation_cost_estimates` reads them; the
  `spend_reconciliation` saved query materializes `invocation_cost_reconciliation`, which LEFT JOINs
  the daily estimate against CUR truth so estimate-vs-billed drift is visible.

The report itself is **not** here. A CUR is account substrate, so it lives in landing-zone's
`org-cost`, and this component reads its bucket, prefix and name from
`/platform/org/cost/cur-export-{bucket,prefix,name}`.

## Inputs

| Variable                                       | Description                                                                                              |
| ---------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `environment`                                  | always `org` — validated, because a workload token here would mean a duplicate copy of the whole account |
| `data_kms_key_arn`, `logs_kms_key_arn`         | the data key encrypts query results and estimates; the log-path key encrypts the publisher's own log group. Both resolve to landing-zone's platform CMK unless that environment sets `separate_logs_key` |
| `tenant_iam_path`                              | IAM path prefix the publisher's tag-read grant is scoped to; verified per cluster by `cost-access`       |
| `athena_results_retention_days`                | how long saved query output is kept (default 30)                                                         |
| `estimate_retention_days`                      | how long per-batch estimate NDJSON is kept (default 90)                                                  |
| `invocation_cost_publisher_log_retention_days` | retention on the publisher Lambda's own logs (default 30)                                                |
| `access_logs_retention_days`                   | retention on S3 server-access logs (default 365)                                                         |
| `imported_model_estimate_usd_per_mtokens`      | per-MTok estimate for custom/imported models, which have no public price (default 0 — no estimate)       |
| `force_destroy_buckets`                        | permit teardown. Two acts: apply with it set, then destroy — it has no effect until an apply lands it    |
| `tags`                                         | common tags                                                                                              |

There is no crawler schedule and no report name. The export's identity comes from landing-zone's SSM
contract, and the table name is a constant this component owns.

There is also no `region`. Everything this component names, places or grants on takes its region
from the provider it is configured with, read once through `data.aws_region.current` — a variable
beside it would be a second answer to a question with one authority, and the two disagreeing is not
an error anything reports.

## Outputs

`cur_export_bucket`, `estimates_bucket_arn`, `athena_workgroup`, `athena_database`,
`athena_results_bucket`, `cur_table_name`, `estimate_table_name`, `reconciliation_view`,
`invocation_cost_publisher_function_name`.

Published to SSM under `/eks-agent-platform/org/cost-pipeline/` for `cost-access` to read and
republish under each cluster's prefix.

## The query the BudgetReconciler issues

Built at runtime, with `<database>` and `<table>` from SSM (`cost-pipeline/athena_database` and
`cost-pipeline/cur_table_name`) and `<platform-id>` from `BudgetPolicy.spec.platformRef.name`:

```sql
SELECT COALESCE(SUM(line_item_unblended_cost), 0) AS spend_usd
FROM "<database>"."<table>"
WHERE COALESCE(element_at(tags, 'resourceTags/PlatformId'),
               element_at(tags, 'iamPrincipal/PlatformId')) = '<platform-id>'
  AND line_item_line_item_type = 'Usage'
  AND line_item_usage_start_date >= date_trunc('month', current_date);
```

Three things about that query are load-bearing:

- **Attribution is a union of two tag prefixes.** CUR 2.0 carries one `tags` map holding every tag
  source at once, keyed by prefix. A tenant's datastores carry `resourceTags/PlatformId`; model
  invocations do not, because an invocation is not a taggable resource — AWS attributes those by
  calling identity under `iamPrincipal/PlatformId`. Either prefix alone returns a plausible number
  that is missing most of the spend.
- **`element_at()`, not `tags['...']`.** Athena is Trino, where the map subscript operator raises on
  a missing key. A line item carrying one prefix and not the other would fail the whole query.
- **`line_item_type = 'Usage'` makes this gross consumption, not net billed.** A credit would offset
  real consumption and hold a runaway tenant under its cap. The switch exists to stop consumption,
  so it counts consumption.

## Both tag keys must be ACTIVATED, and activation is not retroactive

The query above reads two cost-allocation tag keys. Neither column exists in the CUR at all until
the key is **activated in Cost Explorer** — stamping the tag on a resource is not enough. This
component declares that requirement as a `check` block,
`the_cost_allocation_tags_are_active`, over
`required_cost_allocation_tags = ["PlatformId", "iamPrincipal/PlatformId"]`, compared against
`data.aws_ce_tags.observed`. It **warns** on every plan while either key is inactive; it does not
fail, because a component cannot activate a key it does not own and failing would block the apply
that stamps the tag in the first place.

Two properties make the warning worth acting on the day you see it:

- **Activation is payer-level and account-global.** It is not per-cluster, not per-environment, and
  not something a second apply of this component can fix.
- **It is not retroactive.** Every hour before activation is permanently NULL for that key. There is
  no backfill. A tag activated a week after a tenant starts running produces a budget report that is
  simply missing that week, and the gap is invisible in the query result — it reads as low spend.

AWS can take up to 24 hours to *list* a newly observed key, so the key has to be stamped before it
can be activated, and activated before the numbers mean anything. The cost of missing this is paid a
month later, on a partial-month budget report nobody can explain.

`bedrock-account` stamps `PlatformId` and is where the clock starts; see its "Not here" section.

Identifier inputs are validated against `^[a-zA-Z0-9_-]{1,128}$` in the reconciler before
interpolation, and the platform id flows through a single-quote escaper.

A failed query is **not** an error to the reconciler — it is recorded as unreadable spend, which is
why the tests in `tests/` assert the specific values in this path rather than that a plan succeeds.
