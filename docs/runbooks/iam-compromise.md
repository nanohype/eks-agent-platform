# Runbook — Suspected operator-role IAM compromise

**Trigger**: GuardDuty / Detective finding, anomalous CloudTrail activity, leaked credential discovered in a repo or log.

## Immediate (first 5 minutes)

```bash
# 1. Identify the operator role
# <cluster> is the EKS cluster name (e.g. development-platform), not the
# environment token alone — agent-iam publishes under the cluster-keyed tree.
ROLE_ARN=$(aws ssm get-parameter --name "/eks-agent-platform/<cluster>/agent-iam/operator_role_arn" --query 'Parameter.Value' --output text)
ROLE_NAME=${ROLE_ARN#*role/}

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

## Sweep operator-minted roles created during the window

The operator mints three role shapes, and only two of them live under the
tenant path. A path-prefix list misses the rest:

| Shape            | Name formula                            | Path                           | `Component` tag    |
| ---------------- | --------------------------------------- | ------------------------------ | ------------------ |
| tenant           | `<cluster>-<platform>-tenant`           | `/eks-agent-platform/tenants/` | `tenant-iam`       |
| session          | `<cluster>-<platform>-session`          | `/eks-agent-platform/tenants/` | `session-iam`      |
| scheduler-invoke | `<cluster>-<platform>-scheduler-invoke` | `/` (root)                     | `scheduler-invoke` |

Every role the operator creates carries `ManagedBy=eks-agent-platform`. Sweep
by that tag — not by path — so root-path invoke roles show up too.

```bash
# Every operator-minted role (tenant, session, scheduler-invoke), regardless of path.
# resourcegroupstaggingapi is the enumeration surface; IAM list-roles --path-prefix
# cannot see the root-path scheduler-invoke roles.
aws resourcegroupstaggingapi get-resources \
  --resource-type-filters iam:role \
  --tag-filters Key=ManagedBy,Values=eks-agent-platform \
  --query 'ResourceTagMappingList[].[ResourceARN,Tags[?Key==`PlatformId`].Value|[0],Tags[?Key==`Component`].Value|[0]]' \
  --output text \
  > /tmp/operator-roles.txt

# Narrow to roles created during the window (CreateDate is on the role, not the tag).
: > /tmp/suspect-operator-roles.txt
while read -r arn platform_id component; do
  name=${arn##*/}
  created=$(aws iam get-role --role-name "$name" --query 'Role.CreateDate' --output text)
  # ISO-8601 compare: keep anything at or after the window start.
  if [[ "$created" > "<suspect-window-start>" || "$created" == "<suspect-window-start>"* ]]; then
    printf '%s\t%s\t%s\t%s\n' "$name" "$platform_id" "$component" "$created" \
      >> /tmp/suspect-operator-roles.txt
  fi
done < /tmp/operator-roles.txt

# Cross-reference against legitimate Platform CRs in the cluster.
#
# The PlatformId tag is CLUSTER-QUALIFIED — `<cluster>-<platform>`, the same value
# the budget reconciler stamps and filters CUR by — while a Platform CR's name is
# bare. Compare like with like: qualify the CR names with this cluster before the
# set difference, or every legitimate role in the account reads as an orphan and
# this runbook tells you to delete the estate.
#
# The qualification is load-bearing here for the same reason it is in the cost
# path: one account can host several clusters, so an unqualified comparison also
# cannot tell a sibling cluster's live roles from this cluster's orphans.
CLUSTER=$(kubectl get deploy -n agents -l app.kubernetes.io/name=operator \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="AGENTS_CLUSTER_NAME")].value}')
kubectl get platforms -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
  | sed "s|^|${CLUSTER}-|" | sort > /tmp/legit-platforms.txt

# Anything whose PlatformId is not in the cluster is orphan — investigate + delete.
# Scheduler-invoke and session roles share PlatformId with their parent Platform.
#
# Sanity-check before acting: if EVERY role comes back as an orphan, the two sides
# are not in the same namespace of names. Re-read the qualification above rather
# than deleting.
awk '{print $2}' /tmp/suspect-operator-roles.txt | sort -u \
  | comm -23 - /tmp/legit-platforms.txt
```

## Restore service (after audit)

```bash
# Re-provision the operator role via landing-zone agent-iam — the role is
# owned there (not under this repo's terraform/). Stateless apart from the
# role itself; captures the latest baseline policy + trust.
cd ../landing-zone/live/aws/<account>/<region>/<env>/agent-iam
terragrunt apply -auto-approve

# Restart operator pods so they pick up the restored trust
kubectl -n eks-agent-platform rollout restart deploy/operator
```

## Rotate data-plane CMKs if grants were tampered

There is no single `data_kms_key_arn` SSM parameter under agent-iam. Model-
artifacts SSE-KMS uses the cluster data CMK (a landing-zone input to
agent-iam); each tenant's envelope + master-secret key is published by
tenant-substrate at
`/eks-agent-platform/<cluster>/tenant-substrate/<tenant>/kms_key_arn`.

```bash
# Model-artifacts / shared data CMK — ARN from the landing-zone agent-iam
# leaf inputs (or the key's alias in that account). List grants over the
# suspect window:
aws kms list-grants --key-id "<data-cmk-id-or-arn>" \
  --query 'Grants[?CreationDate >= `<suspect-window-start>`]'

# Per-tenant keys the operator grants onto:
for p in $(aws ssm get-parameters-by-path \
  --path "/eks-agent-platform/<cluster>/tenant-substrate/" \
  --recursive --query "Parameters[?ends_with(Name, '/kms_key_arn')].Value" --output text); do
  aws kms list-grants --key-id "$p" \
    --query 'Grants[?CreationDate >= `<suspect-window-start>`]'
done
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
