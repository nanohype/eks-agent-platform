# components/kill-switch

The budget-breach circuit breaker.

When `budget-controller` observes `SpendReport.spend >= BudgetPolicy.threshold * 1.20`, it publishes a `BudgetBreach` event with `severity=critical` to the custom EventBridge bus. The bus rule routes it to a Step Functions state machine that:

1. Detaches the Bedrock-invoke baseline policy from the tenant's IRSA role (`iam:DetachRolePolicy`). Bedrock invocations from that tenant's pods immediately fail authorization.
2. Publishes a `ScaleToZero` event back to the same bus. The operator subscribes via its own EventBridge listener and patches `AgentRuntime.spec.scaling.enabled = false` for every runtime in the breached `Platform`.
3. Logs the entire execution to CloudWatch with `cmk-logs` encryption.

Every event is archived to an EventBridge archive with 365-day retention for compliance.

**Recovery is not automated.** An SSO permission-set elevation (MFA + approver) is required to re-attach the policy. The operator does not have permission to undo a kill-switch.

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
