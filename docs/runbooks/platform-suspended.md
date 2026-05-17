# Runbook — PlatformSuspended

**Severity**: critical. **Pages**: PagerDuty + Slack #incidents + the tenant's persona channel.

## Symptom

A Platform's `status.phase == Suspended`. The tenant IRSA role has been detached from the baseline policy by the kill-switch state machine. Tenant pods can't invoke Bedrock; AgentFleets have scaled to zero.

## Diagnose

```bash
# Which platforms are suspended?
kubectl get platforms -A -o json | jq '.items[] | select(.status.phase=="Suspended") | {name: .metadata.name, tenant: .spec.tenant, reason: .status.suspendedReason, at: .status.suspendedAt}'

# What's the most recent BudgetPolicy reading for the platform?
kubectl get budgetpolicies -A -o json | jq '.items[] | select(.spec.platformRef.name=="<platform-name>")'

# What's on the tenant IAM role tags?
ROLE_NAME=<env>-<platform-name>-tenant
aws iam list-role-tags --role-name "$ROLE_NAME"

# Recent kill-switch SFN executions
aws stepfunctions list-executions --state-machine-arn $(aws ssm get-parameter --name "/eks-agent-platform/<env>/kill-switch/state_machine_arn" --query 'Parameter.Value' --output text) --max-results 10
```

## Most likely cause

Budget breach >= 120% of `BudgetPolicy.spec.monthlyUsd`. The reason tag should say `budget-exceeded`. The status condition `KillSwitchFired` will be set on the BudgetPolicy.

## Verify before un-suspending

1. **Confirm the spend reading is real**: open the AWS Cost Explorer for the relevant time window + filter by `PlatformId` tag.
2. **Confirm the budget was set correctly**: `kubectl get budgetpolicy <name> -n <ns> -o yaml | grep -E "monthlyUsd|killSwitchEnabled"`.
3. **Decide the action**:
   - If the spend is real and the budget was set correctly → raise the budget (a controlled change), OR negotiate cost reduction with the tenant.
   - If the spend is real but the budget was wrong → fix the budget CR, the operator catches up on next reconcile.
   - If the spend is wrong (CUR delay, mis-tagged resources, double-counted in-flight metric) → file a bug, manually un-suspend with the procedure below.

## Un-suspend (manual recovery)

```bash
# Remove the suspension tag from the IAM role
aws iam untag-role --role-name "$ROLE_NAME" \
  --tag-keys agents.stxkxs.io/suspended agents.stxkxs.io/suspended-reason

# Wait for the operator to detect (≤60s with the production reconciler
# requeue interval). Phase flips to Ready.
kubectl get platform <platform-name> -w
```

The next operator reconcile sees the cleared tag, reattaches the baseline policy, restores KMS grant + bucket policy, AgentFleet scales back up. **No CR mutation required for recovery.**

## Postmortem

Required for every Suspended event. Capture:

- platform / tenant / persona / time of suspension,
- spend leading up to the breach (Cost Explorer screenshot),
- whether the alert thresholds (50/80/100%) fired and someone saw them,
- whether the budget was set conservatively enough (suggest doubling next month if breach was legitimate growth),
- whether the SFN itself fired cleanly (check execution logs).
