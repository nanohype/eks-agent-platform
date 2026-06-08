# Runbook — Cluster failover (primary EKS unreachable)

**Trigger**: EKS control plane unreachable for > 10 minutes; AWS support confirms regional impact OR the cluster is in `FAILED` state.

## Pre-conditions

This runbook assumes the multi-cluster topology in [architecture/multi-cluster.md](../architecture/multi-cluster.md) is in place:

- a standby EKS cluster in a separate AWS region,
- ArgoCD hub managing both clusters,
- terraform state remote (S3) so the standby's terraform components are addressable from anywhere.

If the standby doesn't exist, the only recovery is restore-from-state (out of scope; this becomes a hours-long incident).

## Cutover (5-15 minutes)

```bash
# 1. Confirm primary is actually down
kubectl --context=primary get nodes  # should timeout
aws eks describe-cluster --name <primary> --region <primary-region> --query 'cluster.status'

# 2. Promote standby — flip the eks-agent-platform/enabled label on
#    the ArgoCD cluster Secret. Cluster registration lives on the HUB
#    as a Secret in the argocd namespace; the secret name is the
#    cluster server URL by default. Find it:
#      argocd cluster list
#    Then label:
kubectl --context=hub-argocd -n argocd label secret <standby-cluster-secret> \
  eks-agent-platform/enabled=true --overwrite

# 3. That's the whole switch. The eks-gitops ApplicationSets are cluster
#    generators filtered on eks-agent-platform/enabled=true, so flipping the
#    label makes them reconcile the standby in automatically — no manual
#    `kubectl apply` of any ApplicationSet. ArgoCD syncs all addons + the
#    operator onto standby. Watch:
kubectl --context=hub-argocd -n argocd get applications -l 'cluster_name=standby'

# 4. Wait for operator readiness on standby
kubectl --context=standby -n eks-agent-platform wait --for=condition=ready pod -l app.kubernetes.io/name=operator --timeout=10m

# 5. Tenant CRs follow the same path. The portal-tenants ApplicationSet
#    git-sources the tenants gitops repo (tenants/<cluster>/<tenant>.yaml) and
#    is itself a cluster generator on the enabled label, so once the standby is
#    registered + labeled it picks up the tenant manifests and applies them.
#    Confirm they land:
kubectl --context=standby get tenants.platform.nanohype.dev -A
```

## DNS / routing cutover

```bash
# Flip your tenant-facing DNS records to the standby cluster's ALB
# (Route53 weighted records make this a 60s change with TTL=60).
aws route53 change-resource-record-sets --hosted-zone-id <zone> --change-batch '<doc>'
```

Tenant ingress should resume within `tenant TTL + 1 min`.

## What carries over

| Resource                                          | Carries?                                      | How                                                           |
| ------------------------------------------------- | --------------------------------------------- | ------------------------------------------------------------- |
| Tenant / Platform / BudgetPolicy / etc. CRs       | yes                                           | reapply from your gitops source                               |
| Per-Platform IAM roles                            | yes — IAM is global                           | operator reconciles, finds existing roles, no-ops on Create   |
| KMS grants                                        | yes — same KMS key                            | operator reconciles, lists existing grants, no-ops            |
| S3 buckets (artifacts, eval-reports, invocations) | yes — same buckets                            | bucket policy already includes operator role                  |
| Bedrock invocation logs from primary              | yes (in S3 + cmk-logs cw log group, regional) | accessible via Athena from standby once Glue Crawler catalogs |
| In-flight Bedrock requests                        | no                                            | tenants retry against new endpoint                            |

## What needs manual rebuild

- If the standby is in a different region from the primary, Bedrock cross-region inference profile ARNs change. Update `Platform.spec.identity.allowedModels` if you pinned specific inference profile ARNs.
- Athena Glue catalog database is regional — if cost-pipeline was only in the primary region, the standby's Budget reconciler reports zero spend until the Crawler runs. Trigger manually: `aws glue start-crawler --name <crawler> --region <standby-region>`.

## Postmortem

Required for every failover. Capture:

- detection time vs page time,
- standby promotion time (target: < 10 min),
- tenant-visible downtime (DNS TTL + operator-ready time),
- what couldn't be carried over and why,
- whether the runbook actually worked or required deviation.
