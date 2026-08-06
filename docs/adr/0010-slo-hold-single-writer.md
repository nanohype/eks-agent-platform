# ADR 0010 — burn-rate rollout hold: one evaluator, one AppProject writer

## Status

Accepted and implemented (2026-07-24).

`SLOPolicy` (`governance.nanohype.dev/v1alpha1`) is reconciled by
`SLOReconciler`, which evaluates a multi-window burn rate against Amazon Managed
Prometheus, publishes `BurnRateBreach` to the cluster's kill-switch EventBridge
bus, and engages a rollout hold on a page-tier burn. `PlatformReconciler` renders
the hold onto the tenant's ArgoCD `AppProject`.

## Context

The platform already turns one governance signal into an automated action: a
`BudgetPolicy` breach publishes to the kill-switch bus, an EventBridge rule
starts a Step Functions machine, and the machine detaches the tenant's Bedrock
policy (ADR 0003, ADR 0004). Reliability had no equivalent. Burn-rate alerting
existed, but it only ever reached a human: `GrafanaAlertRuleGroup` CRs evaluated
by Amazon Managed Grafana, with the burn factors inlined into PromQL because the
production stack runs no Prometheus ruler.

Three forces shaped the design.

**The evaluation was going to exist twice.** A reconciler that computes a burn
rate to decide an action, next to alert rules that recompute the same burn rate
from the same series to decide a page, is two implementations of one rule. They
drift, and the drift is invisible: the pager and the control loop simply disagree
about whether the tenant is in trouble.

**The action is a Kubernetes write, so EventBridge cannot carry it.** The budget
kill-switch works through the bus because its action is an AWS action — detach a
policy, tag a role. A rollout hold is an ArgoCD object. Routing it through
EventBridge would mean either a Step Functions EKS integration (cluster endpoint
plus an access entry, for one write) or an in-cluster event consumer the operator
does not have.

**The AppProject already has an owner.** `PlatformReconciler.ensureAppProject`
rebuilds the entire `spec` on every tick — `unstructured.SetNestedField` assigns
wholesale at the leaf, and `controllerutil.CreateOrUpdate` issues a full `Update`,
not a patch. Every spec key the reconciler does not itself author is dropped. A
second controller writing a `syncWindow` there is erased within one requeue,
deterministically, not occasionally.

## Decision

**The SLO reconciler is the platform's single evaluator for an objective.** It
queries AMP once per window per tick, computes the multi-window multi-burn-rate
verdict from the factors in `nanohype/standards/observability-slo.json`, and
writes the result to `SLOPolicy.status`. kube-state-metrics projects that status,
so a paging rule reads the number the control loop decided on instead of
re-deriving it. The comparison is strictly greater-than.

> **Amended.** This originally read "matching the operator's own
> PrometheusRule, so the two evaluators cannot disagree at the boundary".
> That rule has been deleted: the catalog installs the prometheus-operator
> CRDs and no ruler, so it was applied everywhere and evaluated nowhere —
> as this ADR itself noted. There is now one evaluator, which is a stronger
> version of the same property.

**The hold is a deny `syncWindow` on the tenant's `AppProject`, authored by the
Platform reconciler.** The SLO reconciler decides and records `HoldEngagedAt`; the
Platform reconciler renders the window from that state, keeping the AppProject
single-writer. A watch on `SLOPolicy` makes the decision reach the writer without
waiting for a resync.

**The event is the routing and audit path, not the action path.** `BurnRateBreach`
carries a distinct source and detail-type from `BudgetBreach`, so the two rule
sets on the shared bus cannot cross-match; rules route critical to the paging
topic and warning to the ticket topic, and the bus archive retains both.
Publishing and acting are independent, so a cluster with no bus configured still
holds — only paging degrades.

**Engaging a hold is not holding.** The SLO reconciler reads the window back every
tick. An engaged hold whose window never appears past a grace window raises
`agents_slo_hold_unobserved_total` and a `RolloutHeld=False/HoldNotObserved`
condition, the same effect-verification the budget kill-switch applies to its own
suspension.

## Consequences

The hold spans two reconcilers, so a wedged Platform reconciler means a decided
hold that never lands. That failure is loud rather than silent — it is exactly
what the unobserved counter and its `SLOHoldUnobserved` alert exist to surface —
and it is preferable to a second writer whose loss is silent and periodic.

Losing telemetry never releases a hold. An observability outage during a bad
rollout is when auto-resuming is worst, so the reconciler leaves an engaged hold
in place when it cannot evaluate. The window keeps `manualSync: true`, so an
on-call engineer is never locked out of shipping the fix.

A cluster without Amazon Managed Prometheus reports `MetricStoreUnavailable` and
evaluates nothing. `enable_managed_monitoring` is opt-in, so this is the common
case today and must not fail operator startup.
