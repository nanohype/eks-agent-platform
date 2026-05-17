# Architecture — Kill-switch flow

End-to-end: budget breach detected → tenant access revoked → fleet scaled to zero → page fired. Loop closes when ops removes the IAM tag.

## Flow

```mermaid
sequenceDiagram
  participant BR as BudgetReconciler
  participant Athena
  participant CW as CloudWatch metric
  participant EB as EventBridge bus
  participant SFN as Step Functions
  participant IAM
  participant PR as PlatformReconciler
  participant AR as AgentFleetReconciler
  participant AM as Alertmanager

  Note over BR: hourly tick
  BR->>Athena: SUM(line_item_unblended_cost)<br/>WHERE platformId=X AND month-to-date
  Athena-->>BR: spend
  BR->>CW: GetMetricData EstimatedInvocationCostUsd<br/>WHERE PlatformId=X (last 24h)
  CW-->>BR: in-flight
  BR->>BR: total = spend + in-flight<br/>pct = total / monthlyUsd * 100

  alt pct >= 120 and killSwitchEnabled
    BR->>EB: PutEvents BudgetBreach<br/>{platformId, spend, pct, reason: 'budget-exceeded'}
    BR->>BR: Status.killSwitchFiredAt = now<br/>condition KillSwitchFired = True
  end

  EB->>SFN: rule 'breach' → StartExecution

  Note over SFN: NormalizeInput Pass
  SFN->>IAM: DetachRolePolicy<br/>· role: env-platform-tenant<br/>· policy: baselineARN
  SFN->>IAM: TagRole<br/>agents.stxkxs.io/suspended=true<br/>agents.stxkxs.io/suspended-reason=$reason

  Note over PR: next reconcile<br/>· within 60s in prod<br/>· RequeueAfter only when IAM client wired
  PR->>IAM: GetRole → tags
  PR->>PR: detect suspended tag<br/>SKIP attachBaselineIfMissing
  PR->>PR: Status.phase=Suspended<br/>Status.suspendedAt=now<br/>Status.suspendedReason

  Note over AR: next AgentFleet reconcile
  AR->>AR: detect Platform.phase=Suspended<br/>cleanupFleetResources()<br/>scale to zero
  AR->>AR: Status.phase=Suspended<br/>condition PlatformSuspended=True

  Note over AM: PlatformSuspended alert fires
  AM->>AM: route critical → PagerDuty + #incidents<br/>+ persona Slack channel
```

## Recovery flow

```mermaid
sequenceDiagram
  participant Ops as Ops engineer
  participant IAM
  participant PR as PlatformReconciler
  participant AR as AgentFleetReconciler

  Ops->>IAM: aws iam untag-role<br/>--tag-keys agents.stxkxs.io/suspended<br/>             agents.stxkxs.io/suspended-reason
  Note over PR: next reconcile<br/>· within 60s in prod<br/>· RequeueAfter only when IAM client wired
  PR->>IAM: GetRole → tags
  PR->>PR: suspended tag absent
  PR->>IAM: attachBaselineIfMissing<br/>· reattaches baseline
  PR->>PR: Status.phase=Ready<br/>Status.suspendedAt=nil
  Note over AR: next AgentFleet reconcile
  AR->>AR: Platform.phase=Ready<br/>ensure SA + NetworkPolicy + kagent Agents + KEDA
  AR->>AR: Status.phase=Ready
```

**No CR mutation required for recovery.** The IAM tag is the on/off switch; the operator reconciles to it.

See [runbooks/platform-suspended.md](../runbooks/platform-suspended.md) for the human-facing playbook including how to verify the spend reading is real before un-suspending.

## Why IAM-tag-based suspension (not k8s-side)

The kill-switch fires from EventBridge, which doesn't speak Kubernetes. Two architectural options:

1. **Lambda subscribed to the event publishes to k8s API** — needs IRSA + cross-cluster auth, brittle for multi-cluster.
2. **Modify AWS state, operator detects drift** — what we shipped. The SFN modifies IAM (which it can); the operator's existing reconcile loop detects the IAM tag and propagates to k8s state.

The second is more loosely coupled. The trade-off: detection lag is the operator's reconcile interval (60s in production). For a 120% budget breach this is fine — the human-meaningful clock is the next page, not the millisecond.

See [ADR 0003 — Threat Model + Cross-component contracts](../adr/0003-threat-model.md#cross-component-contract-kill-switch-suspension-marker) for the canonical tag-key contract.
