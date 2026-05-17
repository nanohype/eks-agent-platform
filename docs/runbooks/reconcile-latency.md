# Runbook — ReconcileLatencyHigh

**Severity**: warning. **Pages**: Slack #oncall only (no PagerDuty).

## Symptom

Operator's reconcile p99 has been above 1s for 10 minutes sustained, for a specific controller (Platform / ModelGateway / AgentFleet / Budget / Eval / Tenant).

## Diagnose

```bash
# Identify which controller
kubectl -n eks-agent-platform logs -l app.kubernetes.io/name=operator --tail=200 | grep "reconcile" | grep -i "took\|latency"

# Check workqueue depth (high depth + high latency = backpressure)
kubectl -n eks-agent-platform port-forward svc/operator-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep -E "workqueue_depth|workqueue_unfinished_work"
```

Each controller has typical bottlenecks:

| Controller     | Likely cause                                                                                                       |
| -------------- | ------------------------------------------------------------------------------------------------------------------ |
| `platform`     | IAM throttling (CreateRole / AttachRolePolicy), KMS CreateGrant rate                                               |
| `modelgateway` | agentgateway CRD create/update latency                                                                             |
| `agentfleet`   | kagent CRD ordering (Agent before ModelConfig)                                                                     |
| `budget`       | Athena query slow (CUR not crawled lately, Glue partition pruning poor)                                            |
| `eval`         | Argo Workflows CRD latency                                                                                         |
| `tenant`       | cluster-wide Platform list — see [reconcile-errors.md](./reconcile-errors.md) for the 1000+ Platforms scaling note |

## Mitigate

1. If a specific AWS API is throttling (look at the operator logs for `Throttling` / `RequestLimitExceeded`), bump that service's account quota or add a backoff. Athena queries can be rerouted to a higher-capacity workgroup.
2. If workqueue depth is climbing without bound, scale the operator deployment: `kubectl -n eks-agent-platform scale deploy operator --replicas=3` and let leader election re-distribute.
3. If a downstream CRD's controller is slow (agentgateway pods saturated, Argo Workflows controller behind), pause new tenant onboarding until the downstream catches up.

## Recover

p99 should fall below 1s within 1-2 reconcile cycles after the bottleneck clears. The alert resolves automatically when 10m of healthy latency reads.

## Postmortem (optional)

If the latency excursion was caused by a misconfigured tenant (e.g., 50 AgentFleets at once from a single tenant), file a [tenant-onboarding policy issue](../onboarding/README.md) — rate-limit large-batch onboarding in `agentctl`.
