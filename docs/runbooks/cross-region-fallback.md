# Runbook — Bedrock region degraded or quota exhausted

**Trigger**: agentgateway logs report sustained `ThrottlingException` or `ServiceUnavailable` from one Bedrock region; tenant pings reporting elevated latency.

## Diagnose

```bash
# Sustained throttling on a specific region?
kubectl -n agentgateway logs -l app.kubernetes.io/name=agentgateway --tail=500 \
  | grep -E "ThrottlingException|ServiceUnavailable|TooManyRequests" \
  | awk '{print $NF}' | sort | uniq -c

# Region-level metrics
aws cloudwatch get-metric-statistics --namespace AWS/Bedrock \
  --metric-name InvocationThrottles --region us-west-2 \
  --start-time $(date -u -d '1 hour ago' +%FT%TZ) --end-time $(date -u +%FT%TZ) \
  --period 60 --statistics Sum
```

## Mitigate via cross-region inference profile

If you're calling a foundation-model ARN directly (e.g., `anthropic.claude-3-5-sonnet-20241022-v2:0`) and one region is throttled, switch to the cross-region inference profile that fans across multiple regions:

```yaml
# In ModelGateway.spec.routes[]:
- name: primary
  modelFamily: anthropic
  modelId: us.anthropic.claude-3-5-sonnet-20241022-v2:0 # cross-region 'us.' prefix
  # crossRegionProfile not needed if modelId already uses the inference profile
```

The `us.` / `eu.` / `apac.` prefixed model IDs are inference profiles that AWS load-balances across multiple regions in the same geo. Most tenants in the examples already use these — verify with `kubectl get modelgateway <name> -o yaml`.

## Hard quota hit (no cross-region relief)

If the inference profile itself is throttled (account-level quota exhausted):

1. **Request a quota increase**: AWS console → Service Quotas → Amazon Bedrock → "Cross-region model invocations per minute". Approval typically same-business-day for production accounts.
2. **Temporarily shrink fleets**: `kubectl -n tenants-<platform> scale deploy/fleet-<name> --replicas=1` for high-volume tenants until the quota lands.
3. **Notify tenants** via #incidents: "Bedrock quota exhausted, expect elevated latency for the next ~30 min".

## A specific tenant is consuming the quota

```bash
# Identify the heavy hitter via the CloudWatch invocation cost metric
aws cloudwatch get-metric-statistics --namespace agents/Bedrock \
  --metric-name EstimatedInvocationCostUsd \
  --dimensions Name=PlatformId,Value=<each-platform> \
  --start-time $(date -u -d '1 hour ago' +%FT%TZ) --end-time $(date -u +%FT%TZ) \
  --period 60 --statistics Sum
```

If one tenant is monopolizing, options:

- lower their `ModelGateway.spec.routes[].rateLimit` (immediate, low-blast-radius),
- raise their `BudgetPolicy.spec.monthlyUsd` to absorb the spike (if it's legitimate growth) AND/OR push them to a less-loaded model variant (Haiku instead of Sonnet for non-deep work).

## Postmortem

Required if tenant SLAs were missed. Cross-region inference profiles should be the default for any production Platform; if a Platform was pinned to a single-region model ARN, file a config-correction ticket.
