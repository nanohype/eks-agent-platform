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

| Tier                  | Control plane (`controlPlaneNamespace`)                          | Workload (`Platform.isolation`) | When                                                                                                                            |
| --------------------- | ---------------------------------------------------------------- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| **Shared** (default)  | `eks-agent-platform` — all tenants' CRs in one platform-owned ns | `namespace` → `tenants-<name>`  | Startup / few tenants. Lowest ceremony.                                                                                         |
| **Dedicated CP ns**   | `eap-tenant-<name>` — per-tenant control-plane ns                | `namespace`                     | Many tenants; per-tenant GitOps Application granularity; per-tenant control-plane RBAC/quota.                                   |
| **vcluster**          | `eap-tenant-<name>`                                              | `vcluster`                      | Hard workload isolation (noisy-neighbor, untrusted code) without a new cluster.                                                 |
| **Dedicated cluster** | (that cluster's mgmt ns)                                         | n/a                             | Regulated / air-gapped / sovereignty. The cluster-scoped `Tenant` + the portal's multi-cluster watcher already anticipate this. |

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
