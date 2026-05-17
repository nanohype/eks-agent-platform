# Grafana dashboards

JSON dashboard models for the eks-agent-platform. Imported via the standard Grafana dashboards-as-configmaps pattern (label `grafana_dashboard: "1"` on the ConfigMap).

## Cross-cutting (op-side)

| Dashboard                                | UID                   | Watches                                                                             |
| ---------------------------------------- | --------------------- | ----------------------------------------------------------------------------------- |
| [tenants.json](./tenants.json)           | `agents-tenants`      | per-tenant rollup, platform ready ratio, % of budget                                |
| [eval-quality.json](./eval-quality.json) | `agents-eval-quality` | EvalSuite pass/fail counts, last-score trend per suite                              |
| [kill-switch.json](./kill-switch.json)   | `agents-killswitch`   | currently suspended platforms, SFN execution count + failures, BudgetBreach events  |
| [agentgateway.json](./agentgateway.json) | `agents-agentgateway` | Bedrock InvokeModel p50/p95/p99, error rate, throughput, throttling, in-flight cost |

## Persona dashboards

| Dashboard                      | UID              | For                                                          |
| ------------------------------ | ---------------- | ------------------------------------------------------------ |
| [finance.json](./finance.json) | `agents-finance` | Aggregate spend, per-tenant cost trend, anomalies            |
| [founder.json](./founder.json) | `agents-founder` | Tenant-of-self view, quick-lookup cost                       |
| [ops.json](./ops.json)         | `agents-ops`     | Operator health (SLO recordings), fleet readiness, SQS depth |

## Wiring

Each dashboard expects two datasources:

- `prometheus` — the in-cluster Prometheus the operator publishes to (via the `ServiceMonitor` shipped in `charts/operator`).
- `cloudwatch` — the Grafana CloudWatch datasource backed by an IRSA role with `cloudwatch:GetMetricData` + `tag:GetResources` permissions.

**Hard dependency**: `kube_customresource_status_phase`, `kube_customresource_status_field`, and `kube_customresource_condition` are emitted by the **`CustomResourceStateMetrics` extension** of kube-state-metrics, NOT stock kube-state-metrics. The CR config ships in [gitops/addons/operator-slo/customresourcestatemetrics.yaml](../addons/operator-slo/customresourcestatemetrics.yaml) as a ConfigMap; your kube-prometheus-stack values overlay must mount it on the kube-state-metrics Deployment via `--custom-resource-state-config-file`. Without that mount every panel and alert that references those metrics silently returns no data — dashboards stay blank, alerts never fire.

Template variables (`$tenant`, `$platform`, `$fleet`, etc.) populate from Prometheus label-values queries against the CR-state metrics; they fall through to "All" when the value list is empty.

## Alert ↔ dashboard mapping

Every alert in [gitops/addons/operator-slo/prometheusrule.yaml](../addons/operator-slo/prometheusrule.yaml) has a panel here that surfaces the same signal:

| Alert                    | Dashboard / panel                                               |
| ------------------------ | --------------------------------------------------------------- |
| `ReconcileLatencyHigh`   | ops.json — reconcile p99                                        |
| `ReconcileErrorRateHigh` | ops.json — operator error rate                                  |
| `OperatorLeaderMissing`  | ops.json — workqueue depth (empty when missing)                 |
| `BudgetReconcileLag`     | finance.json — last-reconciled freshness                        |
| `PlatformSuspended`      | kill-switch.json — currently suspended + suspension table       |
| `TenantBudgetExceeded`   | tenants.json — percent-of-budget by tenant                      |
| `EvalSuiteFailed`        | eval-quality.json — currently failing count                     |
| `KEDASQSTriggerError`    | agentgateway.json — throttling panel + ops.json fleet readiness |
