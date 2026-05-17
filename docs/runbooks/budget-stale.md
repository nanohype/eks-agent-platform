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

# Check the Athena workgroup for failed queries
aws athena list-query-executions --work-group <env>-<cluster>-cost --max-items 20 | jq
```

## Likely causes

1. **Athena query failures** — the CUR table doesn't exist yet (Glue Crawler hasn't run since the CUR was provisioned). Manually trigger: `aws glue start-crawler --name <env>-<cluster>-cost-cur`.
2. **Athena workgroup throttled** — concurrent query quota hit. Increase the workgroup's concurrent query limit or stagger reconcile intervals.
3. **operator role missing Athena perms** — recent IAM regression. See [reconcile-errors.md](./reconcile-errors.md).
4. **CloudWatch GetMetricData throttled** — high-cardinality dimension explosion (rare; the invocation-cost-publisher Lambda dimensions only by PlatformId).

## Mitigate

1. **Crawler missed** — `aws glue start-crawler --name <crawler-name>`. CUR table populates within 5-10 min.
2. **Throttling** — bump Athena workgroup concurrent query limit via `terraform/components/cost-pipeline`.
3. **IAM regression** — rollback or patch the operator role policy.
4. **Manual budget check** — if reconcile is broken but you need spend visibility now: run the rollup query manually in the AWS console with the workgroup + database from `kubectl get cm -n eks-agent-platform operator-config -o yaml`.

## Recover

`lastReconciled` updates on the next successful tick (default 1h in production, 5m in dev). Alert resolves at the next reading.

## Postmortem

Required if the lag exceeded 24h. Risk: a breach went undetected. Cross-check with the CUR for the missed window — manually verify no kill-switch should have fired.
