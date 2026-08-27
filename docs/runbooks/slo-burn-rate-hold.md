# Runbook — SLO burn-rate hold

**Triggers**: the `SLOHoldUnobserved` alert (a hold was decided but never landed), or a
tenant reporting "ArgoCD says my app is blocked by a sync window".

Two different problems share this page. Find out which one you have first.

## What the hold is

An `SLOPolicy` declares one objective. Every 5 minutes the operator's SLO reconciler
queries Amazon Managed Prometheus for the error ratio over each burn-rate window, and
a page-tier burn — both windows of a pair over their factor — does three things:

1. publishes `BurnRateBreach` (source `governance.nanohype.dev/slo`) to the cluster's
   kill-switch EventBridge bus, which pages via the critical SNS topic,
2. sets `status.holdEngagedAt` on the SLOPolicy,
3. leads the Platform reconciler to render a deny `syncWindow` onto the tenant's ArgoCD
   `AppProject`, which stops auto-sync for every Application in that project.

`manualSync` stays open. **A held tenant can still deploy by hand** — the hold stops
automation from advancing a burning rollout, not a human from fixing it.

The hold releases itself when the burn clears. It is deliberately _not_ released when
telemetry is lost.

## First check (60 seconds)

```bash
kubectl get slopolicy -A
kubectl describe slopolicy <name> -n <tenant-namespace>
```

Read the three conditions:

| Condition        | What it answers                                      |
| ---------------- | ---------------------------------------------------- |
| `SLOEvaluated`   | did this tick get a usable reading at all            |
| `BurnRateBreach` | is the budget burning too fast                       |
| `RolloutHeld`    | is the tenant's auto-sync actually stopped right now |

`status.errorRatios` carries the observed ratio per window, so you can see which window
tripped without re-running queries.

## Case 1 — `RolloutHeld=False/HoldNotObserved` (the alert fired)

The platform decided to hold and the hold never took effect. The tenant is still
auto-syncing into a burning SLO. This is the urgent case.

```bash
# Is the AppProject there, and does it carry the window?
kubectl get appproject <platform-name> -n argocd -o jsonpath='{.spec.syncWindows}' | jq
# Is the Platform reconciler running and reconciling?
kubectl logs -n eks-agent-platform deploy/eks-agent-platform-operator | grep -i platform
```

Likely causes, in order:

- **The Platform reconciler is wedged or crash-looping.** It is the only writer of the
  AppProject spec (ADR 0009), so nothing renders the window. Check operator health and
  the `OperatorDown` alert.
- **The SLOPolicy watch is not firing.** The Platform reconciler's periodic resync is
  gated on AWS clients being wired, so on a cluster running without them the watch is
  the only trigger. Confirm the operator's ClusterRole grants `list`/`watch` on
  `slopolicies` — the chart RBAC gate covers this, but an out-of-band install may not.
- **`platformRef` points at a Platform in another namespace or with a different name.**
  `sloHoldWindows` matches on name within the Platform's own namespace.

Immediate mitigation while you fix it — hold the tenant by hand:

```bash
kubectl patch appproject <platform-name> -n argocd --type merge -p '{"spec":{"syncWindows":[
  {"kind":"deny","schedule":"0 * * * *","duration":"2h","timeZone":"UTC","applications":["*"],"manualSync":true}]}}'
```

That is byte-identical to what the operator renders, so the reconciler will recognize it
and the alert will clear.

## Case 2 — `RolloutHeld=True/HoldObserved` (a tenant is blocked, correctly)

The hold is working as designed. Do not remove it to unblock a deploy — fix the burn, or
deploy the fix manually.

```bash
argocd app sync <app> --project <platform-name>   # manualSync is open
```

Read `status.breachedWindow` and `status.errorRatios` to see how fast the budget is
going, and `status.errorBudgetRemaining` for how much is left of the 30-day budget. The
hold lifts on its own within one tick of the burn clearing.

If the objective itself is wrong (a threshold naming a histogram bucket the service does
not publish, a metric renamed), fix the `SLOPolicy` rather than deleting it — a deleted
policy releases the hold and drops the signal.

To opt a tenant out of automated holds while keeping the paging:

```yaml
spec:
  onPageTierBreach: None
```

## Case 3 — `SLOEvaluated=False`

| Reason                   | Meaning                                                                           |
| ------------------------ | --------------------------------------------------------------------------------- |
| `MetricStoreUnavailable` | no AMP endpoint is published for this cluster; `enable_managed_monitoring` is off |
| `NoData`                 | AMP is reachable but the series this SLI names are not in it                      |
| `PlatformNotFound`       | `spec.platformRef` does not resolve in the SLOPolicy's namespace                  |
| `ReconcileFailed`        | the query or an AppProject read errored; the message carries the cause            |

`NoData` on a service that is definitely running usually means the metric name is wrong.
The reconciler builds series names from `spec.sli.metric`: `<metric>_errors_total` and
`<metric>_requests_total` for an availability SLI, `<metric>_request_duration_seconds_bucket`
and `_count` for latency. The name is the OTLP service name with dashes as underscores.

A latency SLI reporting `NoData` while an availability SLI on the same service works
almost always means `thresholdSeconds` names a bucket boundary the histogram does not
publish. That is reported as no-data on purpose: defaulting an absent bucket to zero
would read as "no request was fast" and fabricate a total breach.

**Neither reason releases an engaged hold.** Losing telemetry during a bad rollout is
precisely when resuming it is worst, so the hold stays until the signal comes back and
says the burn cleared. To release it by hand, delete the deny window from the AppProject
— the Platform reconciler will re-render it on the next tick if the hold is still
engaged, so clear `status.holdEngagedAt` too, or fix the signal.

## Rolling the CRD onto an existing cluster

Helm's `crds/` convention never upgrades. `helm upgrade` will not install `SLOPolicy`
onto a release that predates it:

```bash
kubectl apply -f charts/operator/crds/governance.nanohype.dev_slopolicies.yaml
```

## Related

- [ADR 0009](../adr/0009-slo-hold-single-writer.md) — why the evaluation and the write live in different reconcilers
- [kill-switch-fired.md](./kill-switch-fired.md) — the budget analogue, which suspends rather than holds
- [operator-down.md](./operator-down.md) — if neither reconciler is running
