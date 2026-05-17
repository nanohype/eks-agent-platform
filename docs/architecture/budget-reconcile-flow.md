# Architecture — Budget reconcile flow

How `BudgetPolicy.status` gets populated each hour. The reconcile is on a timer (not Watch-driven) because spend doesn't change in a way that maps to k8s events — it accumulates continuously, and the reconciler reads aggregates.

## Tick

```mermaid
sequenceDiagram
  participant BR as BudgetReconciler
  participant K8s as kube-apiserver
  participant Athena
  participant CW as CloudWatch
  participant EB as EventBridge bus
  participant BP as BudgetPolicy.status

  Note over BR: requeue tick<br/>· prod 1h<br/>· dev 5m
  BR->>K8s: Get BudgetPolicy, Get Platform

  BR->>BR: validate identifiers<br/>· database · workgroup · table names<br/>· must match ^[a-zA-Z0-9_-]{1,128}$

  BR->>Athena: StartQueryExecution<br/>SELECT SUM(line_item_unblended_cost)<br/>FROM {database}.{table}<br/>WHERE resource_tags_user_platformid='{name}'<br/>  AND line_item_usage_start_date >= date_trunc('month', current_date)
  Athena-->>BR: query_execution_id

  loop poll until terminal or timeout
    BR->>Athena: GetQueryExecution(qid)
  end

  alt SUCCEEDED
    BR->>Athena: GetQueryResults(qid)
    Athena-->>BR: spend_usd
  else FAILED / CANCELLED / context cancel
    BR->>Athena: StopQueryExecution qid<br/>· deferred — stops billing on orphan query
    BR->>BR: log + fall back to spendCUR=0
  end

  BR->>CW: GetMetricData<br/>namespace=agents/Bedrock<br/>metric=EstimatedInvocationCostUsd<br/>dimension PlatformId={name}<br/>since=now-24h
  CW-->>BR: in-flight values
  BR->>BR: inflight_usd = sum of values<br/>· big.Float preserves sub-cent

  BR->>BR: total = spendCUR + inflight<br/>pct = round(total / monthlyUsd * 100)

  BR->>BR: shouldAlertAt — thresholds, lastPct, currentPct<br/>· handles downward-swing reset

  alt pct >= 120 AND killSwitchEnabled<br/>AND status.killSwitchFiredAt == nil
    BR->>EB: PutEvents BudgetBreach<br/>· detail: platformId, spend, pct, reason<br/>· checks FailedEntryCount, retries on partial fail
    BR->>BP: killSwitchFiredAt = now<br/>condition KillSwitchFired = True
  end

  BR->>BP: currentSpendUsd, percentOfBudget,<br/>lastReconciled, condition BudgetReconciled
```

## Where the inputs come from

```mermaid
flowchart LR
  subgraph aws["AWS account"]
    CUR["AWS Cost & Usage Report<br/>(hourly Parquet → S3)"]
    Crawler["Glue Crawler<br/>(daily 06:00 UTC)"]
    GlueTable["Glue Catalog table<br/>{database}.{table}"]
    Workgroup["Athena workgroup<br/>(KMS-encrypted results)"]

    BL["Bedrock invocation log group<br/>(per-invocation JSON)"]
    Lambda["invocation-cost-publisher<br/>Lambda"]
    CWNS["CloudWatch metric<br/>agents/Bedrock:EstimatedInvocationCostUsd<br/>dim: PlatformId"]
  end

  CUR --> Crawler
  Crawler --> GlueTable
  Workgroup -->|reads| GlueTable

  BL -->|log subscription filter| Lambda
  Lambda -->|PutMetricData| CWNS

  Athena[BR.Athena.StartQueryExecution] -->|via Workgroup| Workgroup
  CW[BR.CloudWatch.GetMetricData] --> CWNS
```

## Why two data sources

| Source                         | Latency                                        | Accuracy                                                                   |
| ------------------------------ | ---------------------------------------------- | -------------------------------------------------------------------------- |
| CUR (via Athena)               | ~24h lag (AWS publishes hourly + ~6h backfill) | authoritative; matches invoice                                             |
| CloudWatch metric (via Lambda) | seconds                                        | estimate (rounded conservatively up via the Lambda's per-1k pricing table) |

The total spend reading is `CUR + CloudWatch`. The kill-switch is intentionally conservative: an estimate that trips slightly early at 120% is cheaper than discovering you breached after the CUR catches up 24h later.

## Operator IAM surface this consumes

| Action                                                                                                     | Resource                                            | Granted by                                             |
| ---------------------------------------------------------------------------------------------------------- | --------------------------------------------------- | ------------------------------------------------------ |
| `athena:StartQueryExecution`, `GetQueryExecution`, `GetQueryResults`, `StopQueryExecution`, `GetWorkGroup` | `arn:aws:athena:*:*:workgroup/<env>-<cluster>-cost` | `terraform/components/cost-pipeline` operator policy   |
| `glue:GetDatabase`, `GetTable`, `GetTables`, `GetPartitions`                                               | catalog + `<env>-<cluster>-cost-cost` database      | same                                                   |
| `s3:GetObject`, `PutObject`, `ListBucket`                                                                  | athena results bucket                               | same                                                   |
| `cloudwatch:GetMetricData`, `GetMetricStatistics`, `ListMetrics`                                           | `*`                                                 | same                                                   |
| `events:PutEvents`                                                                                         | kill-switch event bus ARN                           | `terraform/components/kill-switch` operator bus policy |

See [ADR 0003 — Threat model](../adr/0003-threat-model.md) for the full operator IAM surface enumeration.

## Failure modes

| Failure                                       | Reconciler behavior                                                                                                                    |
| --------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| Athena workgroup not configured (SSM missing) | spendCUR falls back to 0, in-flight reading still used                                                                                 |
| CUR Crawler hasn't run (table doesn't exist)  | Athena query fails → spendCUR=0; runbook [`budget-stale.md`](../runbooks/budget-stale.md)                                              |
| Athena query timeout                          | StopQueryExecution defer fires, query stops billing, reconcile returns and retries next tick                                           |
| CloudWatch GetMetricData errors               | in-flight falls back to 0; CUR-only reading still recorded                                                                             |
| EventBridge PutEvents partial failure         | reconciler detects `FailedEntryCount > 0`, returns error, killSwitchFiredAt not stamped → retries on next tick (no silent breach drop) |
| Context cancel mid-poll                       | StopQueryExecution defer fires, returns context error                                                                                  |
