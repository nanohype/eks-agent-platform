# components/kill-switch

The budget-breach circuit breaker.

When `budget-controller` observes `SpendReport.spend >= BudgetPolicy.threshold * 1.20`, it publishes a `BudgetBreach` event (`source = governance.nanohype.dev/budget`, `detail-type = BudgetBreach`, `severity = critical`) to the custom EventBridge bus. The source string is a cross-language contract with the operator's `budgetEventSource` constant — EventBridge matches it exactly, so a Go test (`budget_killswitch_contract_test.go`) parses this rule's `event_pattern` and fails the build if the two ever drift. The bus rule routes the event to a Step Functions state machine that:

1. Detaches the Bedrock-invoke baseline policy from the tenant's IAM role (`iam:DetachRolePolicy`). Bedrock invocations from that tenant's pods immediately fail authorization.
2. Tags the same role `platform.nanohype.dev/suspended=true` (plus a `-suspended-reason`). The tag is the durable signal the operator reads: without it, the Platform reconciler would notice the detached baseline and reattach it within minutes, undoing the kill-switch.
3. Publishes a `ScaleToZero` notification back onto the bus (archived for compliance and ops visibility) and logs the whole execution to CloudWatch under `cmk-logs`.

Every task retries on backoff and routes to a terminal `RecordFailure` state on exhaustion, so a transient IAM / PutEvents error is recorded to the state machine's CloudWatch execution history (logged at `ALL` with execution data) rather than dropping silently — and any breach that fails to leave the tenant suspended is re-driven by the operator's effect-verifying `KillSwitchUnrouted` net (below), the load-bearing safety net for this path. Every event is archived to an EventBridge archive with 365-day retention for compliance.

The operator closes the loop by **observation, not subscription** — there is no operator EventBridge listener. On its next reconcile the `PlatformReconciler` reads the suspension tag (`iam:GetRole`), moves the Platform to `Suspended`, and the `AgentFleetReconciler` tears the fleet's kagent Agents + KEDA `ScaledObject` down to zero. The `BudgetReconciler` then effect-verifies the outcome: publishing the breach event is not success. A fired kill-switch whose platform is still not `Suspended` after a grace window re-fires the breach (bounded exponential backoff) and raises a `KillSwitchUnrouted` condition + alert, so a broken EventBridge → Step Functions path can never latch as a false success.

**Recovery is not automated.** An SSO permission-set elevation (MFA + approver) is required to clear the suspension tag and re-attach the policy. The operator does not have permission to undo a kill-switch.

## Inputs

| Variable                                | Description |
| --------------------------------------- | ----------- |
| `environment`, `region`, `cluster_name` | identifying |
| `logs_kms_key_arn`                      | cmk-logs    |

The operator role ARN (granted PutEvents on the bus), the tenant IAM path (DetachRolePolicy scope), and the tenant baseline policy ARN (detached on breach) are read in-component from landing-zone's canonical `agent-iam` SSM contract (`/eks-agent-platform/<env>/agent-iam/*`), not passed as inputs.

## Outputs

- `event_bus_name`, `event_bus_arn` — the operator publishes here
- `state_machine_arn` — for ops dashboards
- `archive_name` — for compliance evidence
