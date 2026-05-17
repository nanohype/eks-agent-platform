# Runbooks

Operational playbooks for the eks-agent-platform. Each runbook is referenced by `runbook_url` in `gitops/addons/operator-slo/prometheusrule.yaml`, so pages link directly to the relevant doc.

## Alert-triggered

| Runbook                                          | Triggering alert         | Severity | Persona           |
| ------------------------------------------------ | ------------------------ | -------- | ----------------- |
| [reconcile-latency.md](./reconcile-latency.md)   | `ReconcileLatencyHigh`   | warning  | ops               |
| [reconcile-errors.md](./reconcile-errors.md)     | `ReconcileErrorRateHigh` | critical | ops               |
| [operator-down.md](./operator-down.md)           | `OperatorLeaderMissing`  | critical | ops               |
| [budget-stale.md](./budget-stale.md)             | `BudgetReconcileLag`     | warning  | finance           |
| [platform-suspended.md](./platform-suspended.md) | `PlatformSuspended`      | critical | depends on tenant |

## Scenario-triggered (no automated page)

| Runbook                                                | When                                                       |
| ------------------------------------------------------ | ---------------------------------------------------------- |
| [kill-switch-fired.md](./kill-switch-fired.md)         | A tenant calls in panic that their agents stopped working. |
| [iam-compromise.md](./iam-compromise.md)               | Suspected operator-role compromise; revoke and audit.      |
| [cluster-failover.md](./cluster-failover.md)           | Primary EKS cluster unreachable; promote standby.          |
| [cross-region-fallback.md](./cross-region-fallback.md) | A Bedrock region degrades or quotas exhaust.               |

## Architecture references

- [multi-cluster.md](../architecture/multi-cluster.md) — hub-and-spoke ArgoCD topology, per-cluster vs cluster-wide ApplicationSets, failover semantics.
