# Runbooks

Operational playbooks for the eks-agent-platform. Each is referenced by `runbook_url` on the Grafana-managed alert rules in `eks-gitops/dashboards/base/alerting/`, so a page links directly to the relevant doc. (The catalog installs the prometheus-operator CRDs and no ruler, so a chart-shipped `PrometheusRule` would be applied everywhere and evaluated nowhere — alert rules belong in eks-gitops, not in this repo's charts.)

## Alert-triggered

| Runbook                                          | Triggering alert         | Severity | Persona           |
| ------------------------------------------------ | ------------------------ | -------- | ----------------- |
| [reconcile-latency.md](./reconcile-latency.md)   | `ReconcileLatencyHigh`   | warning  | ops               |
| [reconcile-errors.md](./reconcile-errors.md)     | `ReconcileErrorRateHigh` | critical | ops               |
| [operator-down.md](./operator-down.md)           | `OperatorLeaderMissing`  | critical | ops               |
| [budget-stale.md](./budget-stale.md)             | `BudgetReconcileLag`     | warning  | finance           |
| [platform-suspended.md](./platform-suspended.md) | `PlatformSuspended`      | critical | depends on tenant |
| [vcluster-down.md](./vcluster-down.md)           | `VClusterNotReady`       | warning  | ops               |
| [slo-burn-rate-hold.md](./slo-burn-rate-hold.md) | `SLOHoldUnobserved`      | critical | ops               |

## Scenario-triggered (no automated page)

| Runbook                                                      | When                                                                                                   |
| ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------ |
| [kill-switch-fired.md](./kill-switch-fired.md)               | A tenant calls in panic that their agents stopped working.                                             |
| [iam-compromise.md](./iam-compromise.md)                     | Suspected operator-role compromise; revoke and audit.                                                  |
| [cluster-failover.md](./cluster-failover.md)                 | Primary EKS cluster unreachable; promote standby.                                                      |
| [cross-region-fallback.md](./cross-region-fallback.md)       | A Bedrock region degrades or quotas exhaust.                                                           |
| [deploy-end-to-end.md](./deploy-end-to-end.md)               | Stand up the platform from scratch (local kx or real EKS); or tear it down.                            |
| [import-open-weight-model.md](./import-open-weight-model.md) | Bring an open-weight model onto the platform via Bedrock Custom Model Import and route a tenant to it. |

## Architecture references

- [multi-cluster.md](../architecture/multi-cluster.md) — hub-and-spoke ArgoCD topology, per-cluster vs cluster-wide ApplicationSets, failover semantics.
