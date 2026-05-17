# Runbook — Suspected operator-role IAM compromise

**Trigger**: GuardDuty / Detective finding, anomalous CloudTrail activity, leaked credential discovered in a repo or log.

## Immediate (first 5 minutes)

```bash
# 1. Identify the operator role
ROLE_ARN=$(aws ssm get-parameter --name "/eks-agent-platform/<env>/agent-iam/operator_role_arn" --query 'Parameter.Value' --output text)
ROLE_NAME=$(echo "$ROLE_ARN" | cut -d/ -f2)

# 2. Disable the role's trust policy — operator pods lose AWS access at
#    next STS refresh (≤1 min).
aws iam update-assume-role-policy --role-name "$ROLE_NAME" --policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"sts:AssumeRoleWithWebIdentity"}]}'

# 3. Detach all attached policies (defense-in-depth, in case there are
#    long-lived sessions still using cached credentials)
for p in $(aws iam list-attached-role-policies --role-name "$ROLE_NAME" --query 'AttachedPolicies[].PolicyArn' --output text); do
  aws iam detach-role-policy --role-name "$ROLE_NAME" --policy-arn "$p"
done
```

The operator's reconcile loop stops mutating AWS state. Existing tenant pods continue to function (they use their own tenant IRSA, not the operator role).

## Audit (next 30 minutes)

```bash
# Every AWS call the operator role made in the suspected window
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=ResourceName,AttributeValue="$ROLE_NAME" \
  --start-time <suspect-window-start> --end-time <suspect-window-end> \
  > /tmp/cloudtrail-events.json

# Group by action — anything outside the operator's expected surface
# (CreateRole, AttachRolePolicy, CreateGrant, PutBucketPolicy, PutEvents,
# StartQueryExecution, GetMetricData, etc.) is suspicious.
jq -r '.Events[].EventName' /tmp/cloudtrail-events.json | sort | uniq -c | sort -rn
```

The operator's legitimate API surface is documented in [ADR 0003](../adr/0003-threat-model.md). Compare actual calls to expected.

## Sweep tenant roles created during the window

```bash
# List tenant roles under the operator-managed IAM path
aws iam list-roles --path-prefix /eks-agent-platform/tenants/ \
  --query 'Roles[?CreateDate >= `<suspect-window-start>`].RoleName' --output text \
  > /tmp/suspect-tenant-roles.txt

# Cross-reference against legitimate Platform CRs in the cluster
kubectl get platforms -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort > /tmp/legit-platforms.txt

# Anything in the IAM list but not in the cluster is orphan — investigate + delete
```

## Restore service (after audit)

```bash
# Re-provision the operator role via terraform — stateless apart from
# the role itself. Captures the latest baseline policy + trust.
cd terraform/live/<env>/agent-iam
terragrunt apply -auto-approve

# Restart operator pods so they pick up the restored trust
kubectl -n eks-agent-platform rollout restart deploy/operator
```

## Rotate cmk-data if grants were tampered

```bash
# List active KMS grants on cmk-data over the suspect window
aws kms list-grants --key-id $(aws ssm get-parameter --name "/eks-agent-platform/<env>/agent-iam/data_kms_key_arn" --query 'Parameter.Value' --output text) \
  --query 'Grants[?CreationDate >= `<suspect-window-start>`]'
```

If any grant has an unfamiliar `GranteePrincipal`, revoke it via `aws kms revoke-grant`. If many, rotate the key entirely (out of scope for this runbook — see your org's KMS rotation procedure).

## Postmortem

Required. Capture:

- how the compromise was detected,
- API call timeline (CloudTrail),
- tenant roles created/modified during the window,
- recovery time (operator role disabled → re-provisioned),
- whether the leaked credential made it to a public-readable destination (S3, GitHub, log aggregator),
- corrective: SSO permission boundary tightening, leaked-credential scanner deployment, secret manager adoption.
