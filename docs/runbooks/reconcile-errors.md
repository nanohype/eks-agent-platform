# Runbook — ReconcileErrorRateHigh

**Severity**: critical. **Pages**: PagerDuty + Slack #incidents.

## Symptom

`agents:reconcile_error_rate:5m > 0.05` for 15 minutes — 1-in-20 reconciles are failing.

## Diagnose

```bash
# Tail recent errors, group by controller
kubectl -n eks-agent-platform logs -l app.kubernetes.io/name=operator --tail=500 \
  | grep -i "reconcile failed\|err\|error" \
  | awk '{print $0}' | tail -50

# Check CloudTrail for AccessDenied on the operator role
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=ResourceName,AttributeValue=<operator-role-name> \
  --start-time $(date -u -d '30 min ago' +%FT%TZ) | jq '.Events[] | select(.ErrorCode != null)'
```

## Likely causes (in order of incidence)

1. **Operator IAM regression** — a recent `terraform apply` on `agent-iam` removed a needed permission. Check the most recent `agent-iam` apply; rollback if it landed in the last hour.
2. **AWS service outage** — IAM / KMS / SSM / Athena / EventBridge regional outage. Check the AWS status page for `us-west-2` (or your region).
3. **CRD schema drift** — a recent operator upgrade has a new CRD schema and an existing CR fails validation. Check `kubectl get events -A | grep ValidationFailed`.
4. **Tenant role concurrency** — IAM rate-limit per-account on `CreateRole` (default 100/sec). Unlikely unless a tenant onboarding loop is running.

## Mitigate

1. If cause #1 (IAM regression): `terragrunt apply -auto-approve` the previous good `agent-iam` state (or restore the previous role policy via the AWS console).
2. If cause #2 (AWS outage): nothing the operator can do. Notify tenants via #incidents that platform mutations are paused; mutations resume automatically when AWS recovers.
3. If cause #3 (CRD drift): identify the failing CR via the operator logs and downgrade the operator deployment image to the prior version.

## Recover

Error rate falls within 1-2 reconcile cycles once the root cause is addressed. Alert resolves at 15m of healthy reads.

## Postmortem

Required if cause was a tag-and-roll deployment regression. Document:

- which `terragrunt apply` introduced the regression,
- whether the apply went through the staging environment first,
- whether the operator's IAM coverage tests caught it.
