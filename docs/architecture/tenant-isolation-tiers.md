# Tenant isolation tiers

The platform is an opinionated starting point that has to serve a solo startup
and a regulated enterprise from the same template. The way it does that without
becoming a template you outgrow: **the simple default is a degenerate case of
the scalable model.** Growing up a tier is turning a dial, never a migration or
a rewrite.

This works because three things are already true:

- **`Tenant` is cluster-scoped.** It's the stable identity anchor — the owning
  team. Where a tenant's `Platform` CRs physically live can change; the `Tenant`
  doesn't move, and per-tenant budget/spend roll-up follows it.
- **The operator watches `Platform` cluster-wide.** Where a control-plane CR
  sits is a _placement policy_, not a functional constraint — so namespacing can
  be reshaped with zero operator changes.
- **Isolation is already a spectrum, not a boolean.** `Platform.isolation`
  (`namespace` → `vcluster`) dials workload isolation; `controlPlaneNamespace`
  dials control-plane isolation. They're orthogonal — turn them independently.

## The tiers

A tenant climbs these as count, compliance, or blast-radius needs grow. Earlier
tiers are not "wrong" — they're the right default until a need forces the next.

| Tier                                          | Control plane (`controlPlaneNamespace`)                          | Workload (`Platform.isolation`) | When                                                                                                                            |
| --------------------------------------------- | ---------------------------------------------------------------- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| **Shared** (default)                          | `eks-agent-platform` — all tenants' CRs in one platform-owned ns | `namespace` → `tenants-<name>`  | Startup / few tenants. Lowest ceremony.                                                                                         |
| **Dedicated CP ns**                           | `eap-tenant-<name>` — per-tenant control-plane ns                | `namespace`                     | Many tenants; per-tenant GitOps Application granularity; per-tenant control-plane RBAC/quota.                                   |
| **vcluster** [†](#the-vcluster-rung-adr-0009) | `eap-tenant-<name>`                                              | `vcluster`                      | API-server isolation for tenant code with a k8s token (untrusted logic, tool servers) without a new cluster.                    |
| **Dedicated cluster**                         | (that cluster's mgmt ns)                                         | n/a                             | Regulated / air-gapped / sovereignty. The cluster-scoped `Tenant` + the portal's multi-cluster watcher already anticipate this. |

### The vcluster rung (ADR 0009)

`isolation: vcluster` is a real, reconciled tier. When set, the operator layers a
per-Platform virtual cluster on top of the unchanged host-side provisioning:

- **Install.** The operator declares the vcluster as an ArgoCD `Application` whose
  source is the upstream vcluster chart, pinned to a resolved `0.35.x`
  (`vcluster.chart.version` on the operator chart; Renovate bumps, a human reviews).
  ArgoCD does the Helm install — the operator never runs Helm, the same
  precedent as its `AppProject` declaration. **ArgoCD is a hard prerequisite**: on a
  cluster without it, a vcluster-tier Platform goes to `Failed` with
  `VClusterReady=False` (reason `ArgoCDRequired`) — it never silently downgrades to
  namespace isolation.
- **Drive workloads.** Each workload reconciler (AgentFleet, AgentSandbox, BatchJob,
  ModelGateway, EvalSuite) resolves a **target client** at the top of reconcile: the
  host client for the `namespace` tier, a cached client built from the vcluster's
  kubeconfig Secret for the `vcluster` tier. The reconcile logic is unchanged — only
  the API it writes to moves inside the virtual cluster. kagent + KEDA are
  bootstrapped **inside** each vcluster (via the chart's init-charts), so the fleet's
  control and data plane live entirely within the isolation boundary.
- **Contain from outside.** Every host-side primitive — the `tenants-<name>`
  namespace, its `ResourceQuota`, `LimitRange`, PSS-`restricted`, default-deny
  `NetworkPolicy`/Cilium egress, and the `AppProject` — stays on the host client and
  bounds the vcluster's control-plane pod, its syncer, and every pod the syncer lands
  on the host, from outside the virtual cluster where the tenant cannot reach them.
- **Grant AWS access.** The vcluster syncs the tenant `tenant-runtime` ServiceAccount
  down to the host under a translated name; the operator discovers that name by label
  (cross-checking a byte-identical replica of vcluster's own algorithm) and binds the
  per-Platform IRSA role to it with an EKS Pod Identity association. The syncer's own
  ServiceAccount gets no association, so it has no AWS reach.
- **Deliver tenant apps.** The operator registers the vcluster as an ArgoCD cluster
  Secret scoped to the Platform's `AppProject`, so the tenant's ApplicationSet entry
  targets the virtual cluster like any other destination — the platform-tenant
  contract is unchanged.
- **Teardown.** Finalizer-first, reverse order: delete tenant Applications → the
  ArgoCD cluster Secret → the vcluster Application → the synced-SA Pod Identity
  association (AWS-side state a namespace delete won't reap) → then host cleanup. The
  finalizer will not drop until the vcluster and every host object its syncer created
  are confirmed gone.

**What it buys, and what it doesn't.** vcluster adds **API-server-level** isolation:
tenant code with a Kubernetes token can no longer see the host API at all — a control
distinct from namespace RBAC/network policy, which mediate access to the host API
rather than removing it. It is **not** kernel or node isolation — synced pods share
the same nodes and kernel as every other tenant's, so a container escape is exactly as
available as in the namespace tier. For compute isolation, pair it with the tainted
`AgentSandbox` node pool (an orthogonal dial) or climb to a dedicated cluster. Full
threat delta and the syncer trust boundary: [ADR 0009](../adr/0009-vcluster-isolation-tier.md).

## Why control-plane CRs default to the _shared management_ namespace

The `Platform` / `BudgetPolicy` / `ModelGateway` / `AgentFleet` / `EvalSuite`
CRs _define_ the tenant boundary — budget, allowed models, kill-switch. They are
platform-team-owned control-plane objects, so the default keeps them in
`eks-agent-platform`, **out of the tenant's workload namespace and out of the
tenant's reach.** The operator derives the `tenants-<name>` workload namespace
separately; that's where the tenant's pods (and their RBAC) live.

Deliberately _not_ the default: rendering control-plane CRs into the tenant's
own workload namespace. It co-locates the boundary definition with the workloads
it governs — a privilege-escalation footgun unless the CRD RBAC is airtight.
When a tenant needs control-plane isolation, the answer is a dedicated
_control-plane_ namespace (`eap-tenant-<name>`), still platform-owned — not the
workload namespace.

## Promoting a tenant (no migration)

1. Set `controlPlaneNamespace: eap-tenant-<name>` (and/or `platform.isolation:
vcluster`) for that tenant — a value change in the portal form / template.
2. Re-render + re-apply. ArgoCD moves the CRs to the new namespace; the
   cluster-scoped `Tenant` is untouched, so identity, budget roll-up, and access
   grants carry over.
3. The operator reconciles the Platform from its new home exactly as before
   (cluster-wide watch).

No CRD change, no operator change, no data migration. The dial is the product.
