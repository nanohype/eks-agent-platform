# components/cost-pipeline

Spend visibility for `BudgetPolicy` reconciliation.

## Pieces

- **CUR (Cost & Usage Report)** writes hourly Parquet partitions to a
  KMS-encrypted bucket with `RESOURCES` + `SPLIT_COST_ALLOCATION_DATA`
  schema elements (needed to attribute Bedrock cost back to per-pod /
  per-namespace tags).
- **Athena workgroup + Glue database** for querying the CUR. Query
  results encrypted with `cmk-data`, retention configurable per env.
- **Glue Crawler** runs daily (default `cron(0 6 * * ? *)`) and catalogs
  the CUR Parquet partitions into a table named after the CUR report
  with hyphens normalized to underscores (e.g.
  `eks-agent-platform-dev` → `eks_agent_platform_dev`). The
  `BudgetReconciler` does its own MTD aggregation against this table —
  no separate materialized view is maintained.
- **Invocation-cost-publisher Lambda** subscribes to the Bedrock
  invocation log group (from `components/bedrock`) and republishes
  per-invocation cost as a CloudWatch custom metric
  `agents/Bedrock:EstimatedInvocationCostUsd` dimensioned by
  `PlatformId`. The Budget reconciler reads this for in-flight cost
  estimation (Bedrock invocation logs land in seconds; CUR partitions
  lag by ~24h). Pricing table inside the Lambda is rough and rounded
  conservatively upward; CUR remains authoritative for finance-grade
  billing.
- **Operator policy attachment** — grants the operator role Athena
  query + Glue catalog read + S3 read on Athena results +
  CloudWatch GetMetricData for the invocation cost metric.

## Inputs

| Variable                                | Description                                       |
| --------------------------------------- | ------------------------------------------------- |
| `environment`, `region`, `cluster_name` | identifying                                       |
| `data_kms_key_arn`, `logs_kms_key_arn`  | cmk-data, cmk-logs                                |
| `bedrock_invocation_log_group`          | from `bedrock` outputs — Lambda subscribes here   |
| `cur_report_name`                       | unique per account (default `eks-agent-platform`) |
| `cur_crawler_schedule`                  | cron expression for the CUR Crawler               |
| `athena_results_retention_days`         | per-env: dev 30, staging 90, prod 365+            |

The operator role ARN and name are read in-component from landing-zone's canonical `agent-iam` SSM contract (`/eks-agent-platform/<env>/agent-iam/operator_role_{arn,name}`), not passed as inputs.

## Outputs

- `cur_bucket_arn`, `athena_workgroup`, `athena_database`,
  `athena_results_bucket`, `cur_table_name`,
  `invocation_cost_publisher_function_name`

## Query the BudgetReconciler issues

The reconciler builds this at runtime, with `<database>` and
`<table>` from SSM (`cost-pipeline/athena_database` +
`cost-pipeline/cur_table_name`) and `<platform-id>` from
`BudgetPolicy.spec.platformRef.name`:

```sql
SELECT COALESCE(SUM(line_item_unblended_cost), 0) AS spend_usd
FROM "<database>"."<table>"
WHERE resource_tags_user_platformid = '<platform-id>'
  AND line_item_usage_start_date >= date_trunc('month', current_date);
```

Tag columns get the format `resource_tags_user_<lowercased_tag_name>`
in CUR v1 Athena schema, so the `PlatformId` user tag becomes
`resource_tags_user_platformid`. Identifier inputs are validated
against `^[a-zA-Z0-9_-]{1,128}$` in the reconciler before
interpolation; the value flows through a single-quote escaper.
