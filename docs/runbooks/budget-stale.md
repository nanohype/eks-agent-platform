# Runbook — BudgetReconcileLag

**Severity**: warning. **Pages**: Slack #finance-alerts.

## Symptom

`BudgetPolicy.status.lastReconciled` older than 2 hours. Spend reading is stale; kill-switch may not fire on time if spend is actually breaching.

## Diagnose

```bash
# Identify which BudgetPolicy is stale
kubectl get budgetpolicies -A -o json | jq '.items[] | select(.status.lastReconciled == null or (now - (.status.lastReconciled|fromdateiso8601)) > 7200) | {name: .metadata.name, ns: .metadata.namespace, lastReconciled: .status.lastReconciled}'

# Look at the budget reconciler logs
kubectl -n eks-agent-platform logs -l app.kubernetes.io/name=operator --tail=200 | grep -i "budget"

# Check this cluster's Athena workgroup for failed queries — each cluster has its own
aws athena list-query-executions --work-group <cluster>-cost-access --max-items 20 | jq

# Read the failure reason on the most recent one
aws athena get-query-execution --query-execution-id <id> \
  --query 'QueryExecution.Status.{State:State,Reason:StateChangeReason}'
```

## Likely causes

A failed Athena query is reported as **unreadable spend**, not as a reconcile error — it never
reaches `controller_runtime_reconcile_errors_total`. So the query's own failure reason is the
primary signal, and the command above is the first thing to run.

1. **The CUR table isn't there** — cost-pipeline hasn't been applied for the account, or the
   published name and the declared table disagree. The table is declared, not discovered, so
   there is nothing to trigger and no crawl to wait on:
   `aws glue get-table --database-name <database> --name <table>`. Compare `<table>` against
   `/eks-agent-platform/<cluster>/cost-pipeline/cur_table_name`.
2. **The table exists but has no data** — the export delivers under
   `s3://<bucket>/<prefix>/<export-name>/data/BILLING_PERIOD=YYYY-MM/`. Confirm objects exist for
   the *current* period; at the start of a billing period the partition projects fine and simply
   has nothing in it yet, which reads as $0 rather than as an error.
3. **Athena workgroup throttled** — concurrent query quota hit. Stagger reconcile intervals or
   raise the limit.
4. **operator role missing Athena, Glue, S3 or KMS perms** — recent IAM regression. The role needs
   all four; results are KMS-encrypted, so a missing `kms:GenerateDataKey` fails *after* the scan.
   See [reconcile-errors.md](./reconcile-errors.md).
5. **CloudWatch GetMetricData throttled** — high-cardinality dimension explosion (rare; the
   invocation-cost-publisher Lambda dimensions only by PlatformId).

## Mitigate

1. **Table missing or misnamed** — re-apply `terraform/components/cost-pipeline` (declares the
   table) and then `cost-access` for the cluster (republishes the name under its prefix). A stale
   per-cluster copy of the name is the one way the two can disagree.
2. **Throttling** — there is no concurrency setting on an Athena workgroup, so this is not a
   terraform change. `aws_athena_workgroup.cluster` configures three things and none of them is
   a limit: `enforce_workgroup_configuration`, `publish_cloudwatch_metrics_enabled`, and
   `result_configuration`. Active query concurrency is an **account-level service quota**. Either:
   - stagger the reconcile interval so clusters do not tick together —
     `--budget-requeue-interval` on the operator, surfaced as
     `reconcilers.budget.requeueInterval` in the operator chart; or
   - raise **Service Quotas → Amazon Athena → Active DML queries** for the account, which is a
     support-backed request rather than an apply.

   Diagnose step 3 above already says this. The two halves of this page used to disagree about
   whether the lever existed, and an operator following the old mitigation opened `main.tf`
   during a live incident and found nothing to change.
3. **IAM regression** — rollback or patch the operator role policy.
4. **Manual budget check** — if reconcile is broken but you need spend visibility now: run the rollup query manually in the AWS console with the workgroup + database from `kubectl get cm -n eks-agent-platform operator-config -o yaml`.

## Recover

`lastReconciled` updates on the next successful tick (default 1h in production, 5m in dev). Alert resolves at the next reading.

## Postmortem

Required if the lag exceeded 24h. Risk: a breach went undetected. Cross-check with the CUR for the missed window — manually verify no kill-switch should have fired.
