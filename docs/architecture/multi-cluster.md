# Multi-cluster topology

The eks-agent-platform supports a hub-and-spoke ArgoCD topology for hosting tenants across multiple EKS clusters (geographic, blast-radius, or compliance separation).

## Topology

```
                ┌─────────────────────────────────┐
                │     ArgoCD hub cluster          │
                │  (small, no tenant workloads)   │
                │  • ApplicationSets              │
                │  • Cluster secrets              │
                │  • aggregated dashboards        │
                └──────────────┬──────────────────┘
                               │ sync
              ┌────────────────┼─────────────────┐
              ▼                ▼                 ▼
     ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
     │  Cluster A   │  │  Cluster B   │  │  Cluster C   │
     │  us-west-2   │  │  us-east-1   │  │  eu-west-1   │
     │              │  │              │  │              │
     │  • operator  │  │  • operator  │  │  • operator  │
     │  • agtgw     │  │  • agtgw     │  │  • agtgw     │
     │  • kagent    │  │  • kagent    │  │  • kagent    │
     │  • argo wf   │  │  • argo wf   │  │  • argo wf   │
     │              │  │              │  │              │
     │  Tenants:    │  │  Tenants:    │  │  Tenants:    │
     │  acme,foo    │  │  acme,bar    │  │  baz         │
     └──────────────┘  └──────────────┘  └──────────────┘
              │                │                 │
              └────────────────┴─────────────────┘
                               ▼
                ┌─────────────────────────────────┐
                │       AWS account (per env)     │
                │  • IAM (global)                 │
                │  • KMS keys (regional, 1 each)  │
                │  • S3 buckets (regional)        │
                │  • Bedrock (regional + xR profiles) │
                │  • CUR + Athena (us-east-1 CUR, │
                │    regional workgroups)         │
                └─────────────────────────────────┘
```

## Per-cluster vs cluster-wide concerns

**Per-cluster** (one of each, on every cluster):

- Operator deployment + CRDs.
- agentgateway, kagent, KEDA, Argo Workflows, Argo Rollouts.
- Tenant workload namespaces (`tenants-<platform>`).
- KEDA TriggerAuthentications, ScaledObjects.

**Cluster-wide** (shared across clusters, AWS-global):

- IAM roles (operator + per-tenant).
- KMS grants (per-tenant on the regional cmk-data).
- S3 buckets + bucket policies.
- CUR data (catalogged once in `us-east-1` Athena, queryable from any region's Budget reconciler via cross-region GetMetricData / Athena).

## Cluster registration

Each spoke cluster registers with the ArgoCD hub via a `cluster` secret labeled `eks-agent-platform/enabled=true`. The ApplicationSet generators match on this label so only enabled clusters receive the addon set. Adding a cluster:

```bash
# On the hub. argocd cluster add creates the Secret; labels must be
# added in a separate step (the CLI doesn't accept --label).
argocd cluster add <spoke-context>

# Find the secret (defaults to the cluster server URL)
argocd cluster list
SECRET=$(kubectl -n argocd get secrets -l argocd.argoproj.io/secret-type=cluster \
  -o jsonpath='{.items[?(@.metadata.name=="<spoke-secret-name>")].metadata.name}')

# Label so the ApplicationSet generators select this cluster
kubectl -n argocd label secret "$SECRET" \
  eks-agent-platform/enabled=true \
  cluster_name=<short-name> \
  region=<aws-region> \
  environment=<dev|staging|production>
```

Both `argocd.argoproj.io/secret-type: cluster` (set by `argocd cluster add`) AND `eks-agent-platform/enabled: "true"` (set above) must be present for the ApplicationSet matchLabels to select the cluster. The other labels are surfaced as template variables in the ApplicationSet matrix generators.

## Cluster failover

See [runbooks/cluster-failover.md](../runbooks/cluster-failover.md). Short version: flip the `eks-agent-platform/enabled` label off on the primary, on on the standby; ArgoCD reconciles within ~1 min. Tenant CR re-apply is gitops-driven so the tenants land on the standby on the same sync wave.

## Per-cluster vs per-AWS-account

A common question: should each cluster have its own AWS account?

| Topology                           | Pros                                                                   | Cons                                                                                          |
| ---------------------------------- | ---------------------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| **One AWS account, many clusters** | one CUR, one Bedrock quota, one set of KMS keys, one operator IAM role | blast radius — an operator regression affects every cluster                                   |
| **One AWS account per cluster**    | hard cost + IAM + KMS blast-radius separation                          | duplicate quotas (annoying for Bedrock cross-region inference profiles), duplicate CUR setups |

The terraform components are designed for the one-account-many-clusters case. If you split per-account, run `terragrunt apply` per account against its own `terraform/live/<env>/` mirror with separate state backends.

## In-flight request handling on failover

Bedrock invocations are stateless from the platform's perspective: the agentgateway pod that's calling Bedrock just times out if the cluster goes away. Tenants' retry logic handles this (they're invoking through their own SDK; nothing platform-side is in flight). Argo Workflows in-flight runs are _not_ portable across clusters — they fail on the primary, the eval-runtime on the standby starts fresh. Accept this; don't try to migrate Workflow state mid-execution.

## Cross-cluster persona dashboards

The Tenant CR is cluster-scoped, so each cluster has its own Tenant list. If you want a single pane of glass across clusters:

- The Grafana dashboards query Prometheus federated from each cluster's prom-stack — single dashboard view.
- For CLI: `agentctl tenant list` runs against one cluster's context; loop over contexts for a federated view (`for ctx in $(kubectl config get-contexts -o name); do kubectl config use-context $ctx; agentctl tenant list; done`).

A future "control plane" tenant overview would centralize the Tenant CRs in the hub cluster and project them down — out of scope today, but the spoke-local pattern doesn't preclude it.
