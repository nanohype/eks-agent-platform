# ADR 0009 — vcluster hard-isolation tier: reconcile model, synced-SA Pod Identity, ArgoCD destination, containment, teardown

## Status

Accepted and implemented (2026-07-17).

This ADR is the design of record for `Platform.spec.isolation: vcluster`. The
reconciler implements it: the CRD accepts the value
(`operators/api/platform/v1alpha1/platform_types.go`), the fail-closed reconcile
path (ArgoCD-declared install, client swap, synced-SA Pod Identity binding,
finalizer-ordered teardown) is in place, and `SECURITY.md` and
`docs/architecture/tenant-isolation-tiers.md` describe it as a real, reconciled
tier. Conformance tests and validation against a live `kx` cluster confirm a
tenant declaring `vcluster` receives API-server-level isolation, not namespace
isolation silently substituted underneath it.

## Context

`Platform.isolation` is the workload-isolation dial of the tenant-isolation model
(`docs/architecture/tenant-isolation-tiers.md`). Two rungs exist:

- `namespace` (default) — the operator provisions a `tenants-<platform>` workload
  namespace with a `ResourceQuota`, `LimitRange`, PSS-`restricted` enforcement, a
  default-deny `NetworkPolicy` (plus the tenant egress allow-list / Cilium egress),
  an ArgoCD `AppProject`, and a per-Platform IRSA role bound to the `tenant-runtime`
  ServiceAccount through an EKS Pod Identity association. Tenant workloads share the
  host API server; isolation is namespace RBAC + network policy.
- `vcluster` — the same host-side containment, plus a **per-Platform virtual
  Kubernetes cluster** so tenant code that talks to the Kubernetes API talks to its
  own API server, not the host's.

The value of the second rung is specific and narrow: it removes the host API server
from the tenant's view. That is a different control from namespace RBAC/network
policy, and it is the correct answer for one problem — tenant code (an agent, a
tool server, untrusted user-supplied logic) that holds a Kubernetes API token and
could otherwise probe the host API. It is **not** a compute boundary; the threat
delta below is explicit about what it does and does not buy, because `SECURITY.md`
currently oversells it as "kernel-level."

[vcluster](https://github.com/loft-sh/vcluster) is open source (Apache-2.0) and its
architecture already matches this platform's grain: a virtual control plane runs as
a pod inside a host namespace, a **syncer** copies a curated set of the virtual
cluster's objects (pods, services, endpoints, secrets, configmaps, and — when
enabled — service accounts) down into that one host namespace, and the host
scheduler runs the resulting pods. The virtual cluster is cheap (one control-plane
pod, no new nodes) and its pods remain ordinary host pods subject to host admission
and host policy. That is exactly the containment story this platform already builds
per Platform — so vcluster composes with it rather than replacing it.

This ADR resolves the design questions that must be settled before that reconcile
path is written: how the operator installs and drives the vcluster, how the
per-Platform IRSA role reaches the tenant's pods once the syncer has rewritten their
service-account names, how ArgoCD delivers tenant Applications into the virtual
cluster, what the virtual cluster and its syncer add to and subtract from the threat
model, and how a deleted `Platform` tears the whole thing down without orphaning a
running vcluster or the host objects it synced.

## Decision

When `spec.isolation == "vcluster"`, the operator layers a per-Platform virtual
cluster on top of the unchanged host-side Platform provisioning:

1. **Install** — the operator **declares** the vcluster as an ArgoCD `Application`
   (a rendered `unstructured` object, the same mechanism it already uses for the
   `AppProject`) whose source is the upstream `vcluster` Helm chart pinned to a
   resolved `0.35.x`, destination `tenants-<platform>`, scoped to the Platform's
   `AppProject`. ArgoCD performs the Helm render+install. The operator never shells
   out to Helm and never renders the vcluster's own manifests.
2. **Drive workloads** — the workload reconcilers (AgentFleet, AgentSandbox,
   BatchJob, ModelGateway, EvalSuite) resolve a **target client** at the top of
   reconcile: the host client for the `namespace` tier, a cached client built from
   the vcluster's kubeconfig Secret for the `vcluster` tier. The reconcile _logic_
   is unchanged; only the API it writes to moves inside the virtual cluster.
3. **Contain from outside** — every host-side primitive stays on the host client and
   keeps its meaning: the `tenants-<platform>` namespace, its `ResourceQuota`,
   `LimitRange`, PSS-`restricted`, default-deny `NetworkPolicy`/Cilium egress, and
   the `AppProject`. They bound the vcluster control-plane pod, the syncer, and every
   pod the syncer lands on the host — from outside the virtual cluster, where the
   tenant cannot reach them.
4. **Grant AWS access** — the operator enables `sync.toHost.serviceAccounts.enabled`
   so the tenant `tenant-runtime` ServiceAccount materializes on the host, then binds
   the **synced host ServiceAccount** (not the virtual name) to the per-Platform IRSA
   role via the existing Pod Identity association primitive.
5. **Deliver tenant apps** — the operator registers the vcluster as an ArgoCD cluster
   Secret scoped to the Platform's `AppProject`, so the tenant's ApplicationSet entry
   targets the virtual cluster as an ArgoCD destination.
6. **Tear down finalizer-first** — the Platform finalizer removes the vcluster
   Application, the cluster Secret, and the synced-SA Pod Identity association, and
   verifies the vcluster and its synced host objects are gone, before it drops — so a
   deleted Platform can orphan neither the virtual cluster nor anything it synced.

The rest of this ADR is the detail behind each point.

## Reconcile model

### Install: an ArgoCD Application, not Helm-in-operator, not hand-rendered manifests

The operator has one consistent way of standing up Kubernetes objects it owns: it
renders them programmatically — typed client-go objects for the resources it knows
(`Namespace`, `ResourceQuota`, `NetworkPolicy`), `unstructured.Unstructured` for
foreign CRDs it deliberately keeps out of its dependency graph (the `AppProject`,
`operators/internal/controller/platform_reconcile.go:286`), and verbatim
`.Files.Get` emission from `charts/operator/files/` for CR-heavy static manifests it
does not want Helm to evaluate (the eval-runtime + SLO bundles, ADR 0008). It does
**not** shell out to the Helm CLI anywhere, and it does not embed Helm's Go SDK.

A per-Platform vcluster is a full Helm release — a StatefulSet, the control-plane and
syncer, RBAC, CRDs, CoreDNS, and generated certificates — pinned to an upstream chart
that changes across releases. Three ways to install it were weighed:

- **Operator embeds Helm (SDK or subprocess).** Rejected. The Go SDK pulls a large
  dependency tree into a binary whose current dep graph is deliberately narrow; the
  subprocess variant needs the `helm` binary baked into the operator image plus chart
  pull and repo credentials at reconcile time — a new runtime + supply-chain surface.
  Either breaks the "operator renders its own resources, never Helm" idiom that ADR
  0008 chose on purpose.
- **Operator hand-renders the vcluster's manifests** into `charts/operator/files/`
  and templates them per Platform (the eval-runtime pattern). Rejected. It forces the
  operator to track and faithfully reproduce vcluster's entire manifest surface —
  StatefulSet, RBAC, CRDs, cert generation, CoreDNS — and re-derive it on every
  upstream bump. The eval-runtime manifests are a handful of static CRs the operator
  authored; a vcluster release is a moving upstream product. Wrong tool.
- **Operator declares an ArgoCD `Application`** whose source is the upstream chart,
  and lets ArgoCD render+install it. **Chosen.**

The Application approach fits the existing grain exactly. The operator already writes
an `unstructured` ArgoCD object per Platform (the `AppProject`); the `Application` is
the same act with a Helm source. ArgoCD is already the cluster's Helm render+deploy
engine and already the delivery path this platform is built around
(`nanohype/standards/platform-tenant-contract.json` ships every app as an
ApplicationSet entry). Version is a pinned `spec.source.targetRevision`, which makes
the upgrade story fall out for free (see Failure modes). The operator's dependency
graph stays clean — it gains no Helm code and no vcluster Go types.

The Application's source is the `vcluster` chart from `https://charts.loft.sh`
(equivalently the OCI mirror `oci://ghcr.io/loft-sh/charts/vcluster`), pinned to the
resolved chart version (see Version). Its `spec.source.helm.valuesObject` carries the
per-Platform `vcluster.yaml` this ADR specifies (single-namespace mode, service-
account sync on, the syncer's host RBAC left namespace-scoped). Its destination is
`tenants-<platform>`; its `project` is the Platform's `AppProject`. The chart's
`spec.source.repoURL` must be added to the `AppProject.sourceRepos` allow-list
alongside the existing `github.com/nanohype/*` and
`oci://ghcr.io/nanohype/eks-agent-platform/charts/*` entries.

**ArgoCD is a hard prerequisite for this tier.** The host-tier `ensureAppProject`
tolerates ArgoCD being absent (`isNoKindMatch` → skip,
`platform_reconcile.go:169`), because namespace isolation does not need it. The
vcluster tier does: no ArgoCD, no way to install the vcluster or register its
destination. On a cluster without the ArgoCD CRDs, a Platform requesting
`isolation: vcluster` must go to a `Failed`/degraded phase with a clear condition —
**it must not silently fall back to namespace isolation.** Silent fallback is the
exact defect this whole tier exists to remove; the reconcile path fails closed
instead.

### Drive workloads: a target-client swap, not a parallel reconcile path

Once the vcluster is up, the workload CRDs the operator reconciles from
(`AgentFleet`, `AgentSandbox`, `BatchJob`, `ModelGateway`, `EvalSuite`) must land
their objects — kagent `Agent`/`ModelConfig`/`ToolServer`, KEDA `ScaledObject`,
sandbox pods — inside the virtual cluster's API, so that the tenant's pods see the
vcluster API server rather than the host's.

The reconcile _logic_ (what objects a fleet or sandbox decomposes into) is identical
regardless of tier; only the _target API_ differs. So the seam is a **client swap at
the top of each workload reconcile**, not a duplicated code path:

```
func (r *Reconciler) targetClient(ctx, platform) (client.Client, error)
    isolation == "namespace" → r.Client                    (host, as today)
    isolation == "vcluster"  → r.vclusterClientFor(platform) (built from the
                               vcluster kubeconfig Secret in tenants-<platform>)
```

Each workload reconciler resolves its owning Platform (the `resolve*Platform` helpers
already do this), calls `targetClient` once, and writes all in-cluster objects
through the returned client. A parallel per-tier reconcile path was rejected: it would
duplicate every fleet/sandbox/gateway reconcile and drift the two copies. The swap is
one function and one seam.

This matches the repo's existing testability idiom — the AWS clients (`IAM`, `EKS`,
`KMS`, `S3`) are interface-injected and nil-in-tests
(`platform_controller.go:52-55`, faked via the awsclients pattern). The vcluster
client is another injected seam: a `vclusterClientFor` factory faked in envtest so
the swap is exercised without a real virtual cluster, and validated for real on kx.
The factory builds a controller-runtime client from the kubeconfig Secret vcluster
publishes (`vc-<name>` in the host namespace), caches it per Platform keyed on the
Secret's `resourceVersion`, and invalidates on rotation or vcluster restart. The
operator runs as a single leader (leader election), so a process-local cache is
sufficient — the same reasoning that lets `bucketPolicyMu` be a process-local mutex
(`platform_controller.go:64-70`).

**Open for implementation (Target 7):** the fleet's in-cluster dependencies — the
kagent CRDs+controller and KEDA — must be present _inside_ the virtual cluster for
the operator to create `Agent`/`ScaledObject` objects there and have them reconcile
to pods. The recommended default is to bootstrap them into the vcluster via the
chart's init-manifests/init-charts, so the tenant's fleet control plane and data
plane live entirely inside the isolation boundary (the honest "hard isolation"
shape: the tenant gets its own API server, its own kagent, its own KEDA). The
alternative — sync the host's kagent/KEDA CRDs into the vcluster and let host
controllers act on them — leaks the host control plane back into the boundary and is
not recommended. Per-tenant controller overhead is the accepted cost of climbing to
this tier; the host `ResourceQuota` caps it. kx validation confirms the choice.

### Contain from outside: host primitives are unchanged and still load-bearing

Every host-side primitive the `PlatformReconciler` provisions stays on the host
client and keeps operating on `tenants-<platform>` exactly as in the namespace tier
(`platform_controller.go:155-165`): the namespace with PSS-`restricted` enforced at
admission, the `ResourceQuota` + `LimitRange`, the default-deny `NetworkPolicy` and
tenant Cilium egress, and the `AppProject`. In the vcluster tier these are not
redundant — they are the containment layer, and they now bound three new things from
outside the virtual cluster, where the tenant has no reach:

- the vcluster **control-plane pod** (subject to PSS-`restricted` and the quota),
- the **syncer** (its host creates are subject to host admission and network policy),
- every **synced pod** the syncer lands on the host (real host pods in
  `tenants-<platform>`, subject to the quota, the PSS profile, and the default-deny
  egress — a synced pod cannot exceed the tenant quota, escape `restricted`, or open
  egress the tenant `NetworkPolicy`/Cilium policy denies).

This composes cleanly because the containment controls are host-namespace admission
and policy, and the syncer's outputs are ordinary host-namespace objects. The
virtual cluster changes what the tenant _sees_ (its own API); it does not change what
the host _enforces_ on the pods that result.

## Pod Identity for synced service accounts

### The problem the syncer creates

In the namespace tier the operator binds the tenant `tenant-runtime` ServiceAccount
to the per-Platform IRSA role with a Pod Identity association keyed on
`(cluster, namespace=tenants-<platform>, serviceAccount=tenant-runtime)`
(`ensurePodIdentityAssociation`, `platform_iam.go:283`). EKS Pod Identity resolves
credentials by the pod's **host** `(namespace, serviceAccountName)`.

Under a vcluster the pod the tenant creates lives in the _virtual_ cluster. The
syncer copies it to the host under a translated name, and — this is the crux —
rewrites its `serviceAccountName`. A Pod Identity association against the virtual name
`tenant-runtime` in `tenants-<platform>` would match nothing, because no host pod
runs under that host `(namespace, serviceAccount)` pair. The association must target
the **synced host ServiceAccount name**.

### Enable service-account sync

By default vcluster does not sync ServiceAccounts to the host; synced pods run under a
vcluster-managed workload SA that is not individually addressable per tenant SA. The
operator therefore sets, in the per-Platform `vcluster.yaml`:

```yaml
sync:
  toHost:
    serviceAccounts:
      enabled: true
```

With this on, the virtual `tenant-runtime` SA materializes as a host ServiceAccount in
`tenants-<platform>`, and the syncer rewrites synced pods' `serviceAccountName` to
that host name — which is what makes per-SA Pod Identity possible at all (this is the
mechanism the upstream EKS-Pod-Identity-with-vcluster integration relies on).

### The synced host name: discover it, then verify against the computed name

vcluster's host name for a synced namespaced object is deterministic. From
`pkg/util/translate` (v0.35.x):

```go
SingleNamespaceHostName(name, namespace, suffix) = SafeConcatName(name, "x", namespace, "x", suffix)

SafeConcatName(parts...) string {
    full := strings.Join(parts, "-")
    if len(full) > 63 {
        digest := sha256.Sum256([]byte(full))
        return full[0:52] + "-" + hex.EncodeToString(digest[:])[0:10]   // 52 + 1 + 10 = 63
    }
    return full
}
```

So the synced host SA name is
`SafeConcatName("tenant-runtime", "x", <virtual-namespace>, "x", <vcluster-name>)`
— order **name-x-namespace-x-vcluster** — capped at 63 characters, the Kubernetes
ServiceAccount name (DNS-label) limit and the length Pod Identity accepts.

**Primary: discover the name from the host, don't recompute it.** vcluster labels
and annotates every synced host object with its provenance
(`pkg/util/translate/types.go`):

| key                                            | value on the synced SA          |
| ---------------------------------------------- | ------------------------------- |
| label `vcluster.loft.sh/managed-by`            | `<vcluster-name>`               |
| annotation `vcluster.loft.sh/object-name`      | `tenant-runtime` (virtual name) |
| annotation `vcluster.loft.sh/object-namespace` | `<virtual-namespace>`           |
| annotation `vcluster.loft.sh/object-host-name` | the SA's own host name          |

The operator lists ServiceAccounts in `tenants-<platform>` with label selector
`vcluster.loft.sh/managed-by=<vcluster-name>`, filters to the one whose
`object-name`/`object-namespace` annotations match the tenant SA, and reads its
`metadata.name`. Discovery is robust to vcluster changing its internal naming
algorithm across versions — a real risk, since `SafeConcatName` is vcluster-internal.
It is a two-phase reconcile: enable SA sync → requeue until the synced SA appears →
create the association. The reconciler already tolerates multi-pass convergence (the
finalizer requeue, the 60s resync at `platform_controller.go:316`); the vcluster tier
adds one more converge-and-requeue.

**Cross-check: compute the expected name and assert the discovered SA matches it.**
The operator carries a `syncedHostSAName(virtualNs, vclusterName)` helper that
replicates `SafeConcatName` exactly, and asserts `discovered == computed`. A mismatch
means vcluster changed its scheme on an upgrade → surface a condition and fail loud,
rather than bind the wrong SA and hand Pod Identity a target that resolves nothing.

### Length math — the same discipline as `platform_iam.go`, a different algorithm

The operator already caps every identifier at the platform's hard limit by
concat-then-hash-truncate:

| name                     | limit              | idiom                                                                                 | source                                                 |
| ------------------------ | ------------------ | ------------------------------------------------------------------------------------- | ------------------------------------------------------ |
| tenant IAM role          | 64 (IAM)           | `<cluster>-<name>-tenant`, else prefix + `name[:budget]` + `-<8-hex FNV-1a>` + suffix | `platform_iam.go:75-87`                                |
| tenant host namespace    | 63 (DNS label)     | `tenants-<name>`, else `tenants-<name[:46]>-<8-hex FNV-1a>`                           | `PlatformNamespace`, `platform_reconcile.go:50-63`     |
| **synced host SA (new)** | **63 (DNS label)** | `SafeConcatName(...)` — `full`, else `full[0:52]-<10-hex SHA-256>`                    | vcluster `translate`, replicated in `syncedHostSAName` |

The new name obeys the same law — concatenate, and if the concatenation exceeds the
identifier's hard limit, keep a prefix and append a hash — so it provably fits: the
truncated form is `52 + 1 + 10 = 63 ≤ 63`, and the untruncated form is `≤ 63` by the
guard. The one thing the implementer must not do is reuse the operator's `fnv1a64` /
`tenantRoleName` to compute this name. vcluster is the _writer_ of the synced SA, so
**vcluster's algorithm is authoritative** — SHA-256, prefix 52, hash 10 — not the
operator's FNV-1a-8. A naive reuse of the existing idiom would compute a different
string, the association would bind nothing, and the tenant's pods would silently fail
to obtain credentials. Same discipline, different — and non-negotiable — algorithm.

Choosing a fixed, short `<vcluster-name>` (recommend the literal `vcluster`, one per
tenant namespace) and a fixed, short virtual namespace for the tenant SA keeps the
common case in the un-truncated, human-legible regime:

- happy path — `tenant-runtime-x-agents-x-vcluster` = 34 chars, no hash, stable and
  readable;
- worst case — a maximal 63-char virtual namespace pushes the concatenation over 63,
  `SafeConcatName` returns `<first 52>-<10-hex>` = 63 chars, still deterministic and
  unique (SHA-256), still within the limit.

The IAM role name itself is unchanged across tiers (`<cluster>-<platform>-tenant`,
`tenantRoleName`); only the association's `serviceAccount` argument moves from
`tenant-runtime` to the synced host name. The `ensurePodIdentityAssociation` /
`deletePodIdentityAssociation` primitives take `(namespace, serviceAccount)` already —
the vcluster tier passes `(tenants-<platform>, <synced-host-SA>)` instead of
`(tenants-<platform>, tenant-runtime)`; no new primitive is needed.

## ArgoCD destination

Tenant applications ship as an ApplicationSet entry and are delivered by ArgoCD, not
by the operator (`platform-tenant-contract.json` — every app is a chart + an
`applicationset-entry.yaml`). For a `vcluster`-tier Platform, that GitOps-delivered
app chart must land in the **virtual** cluster. Two ways were weighed:

- **Register the vcluster as an ArgoCD cluster Secret, scoped to the Platform's
  `AppProject`.** ArgoCD targets non-default clusters via Secrets labelled
  `argocd.argoproj.io/secret-type: cluster` that carry the target API endpoint and
  credentials. The operator already owns the per-Platform `AppProject`; it also
  translates the vcluster kubeconfig Secret (`vc-<name>` in `tenants-<platform>`) into
  an ArgoCD cluster Secret whose server is the in-cluster vcluster endpoint
  (`https://<vcluster-service>.tenants-<platform>.svc`), reachable by the in-cluster
  ArgoCD. The Platform's `AppProject.destinations` gains that server (scoped so this
  tenant can deploy only into its own vcluster, not the host and not another tenant's
  vcluster); the tenant's ApplicationSet entry sets its destination to the registered
  cluster. **Chosen.**
- **Operator applies the tenant's resources into the vcluster directly**, bypassing
  ArgoCD cluster registration. Rejected. The tenant's _app chart_ is GitOps-owned by
  contract; having the operator apply it would split delivery into two mechanisms
  (operator for vcluster-tier apps, ArgoCD for everyone else) and break the "every app
  is an ApplicationSet entry" contract.

Note the deliberate split, which mirrors the host tier exactly:

- **operator-owned control objects** (kagent `Agent`, `ModelConfig`, KEDA
  `ScaledObject`, sandbox pods) reconcile into the vcluster through the **target-client
  swap** — they are reconciled from CRs, not GitOps-delivered;
- **tenant-owned app chart** reconciles into the vcluster through **ArgoCD via the
  cluster Secret** — it is GitOps-delivered.

Both write to the vcluster API; only the writer differs. This is the same division of
labor as the namespace tier, where the operator writes IAM/namespace/quota and ArgoCD
writes the app chart — lifted intact onto the virtual cluster's API. It honors the
platform-tenant contract (the app still ships as a chart + ApplicationSet entry, the
ServiceAccount still carries no role-arn annotation, Pod Identity still binds it) and
matches how this repo already integrates with ArgoCD/eks-gitops — the operator writes
the `AppProject`, eks-gitops's ApplicationSet drives the Applications.

## Threat delta (STRIDE)

Evaluated against ADR 0003's frame. The vcluster tier is a **defense-in-depth API
boundary layered on** the namespace tier's controls, plus one new privileged
component (the syncer).

### What the virtual cluster adds

| Threat mitigated                                                                                                 | STRIDE | How the virtual cluster helps                                                                                                                                                                                                                                                  |
| ---------------------------------------------------------------------------------------------------------------- | ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Tenant code with a k8s API token enumerates host objects (other tenants' CRs, Secrets, the operator's resources) | **I**  | The tenant's in-cluster kubeconfig points at the **vcluster** API. The host API is not in the tenant's view at all — it cannot `list`/`get` host objects even to try. Distinct from namespace RBAC, which governs authz _on the host API_; here the host API is simply absent. |
| A host-RBAC misconfiguration on the tenant SA escalates to cross-tenant reach                                    | **E**  | The tenant SA the tenant's code uses is a **virtual** SA; its RBAC is the vcluster's, scoped to the vcluster. A host-side RBAC bug on `tenant-runtime` is unreachable by tenant code, because tenant code isn't calling the host API. Defense in depth over the host RBAC.     |
| Tenant impersonates a host ServiceAccount / spoofs a host identity                                               | **S**  | No host API surface to spoof against from inside the virtual cluster.                                                                                                                                                                                                          |
| Tenant probes cluster-scoped host resources (nodes, other namespaces, CRDs)                                      | **I**  | The virtual cluster presents its own cluster-scoped surface; host cluster-scoped objects are not visible.                                                                                                                                                                      |

The single sentence: **vcluster adds API-server-level isolation for any tenant code
that talks to the Kubernetes API — a control the namespace tier's network policy and
RBAC do not provide, because they mediate access to the host API rather than removing
it.**

### What the virtual cluster does NOT add

- **No kernel or node isolation.** Synced pods run on the **same host nodes, the same
  kernel, the same container runtime** as every other tenant's pods. A container
  escape or kernel exploit is exactly as available to a vcluster-tier pod as to a
  namespace-tier pod — it lands the attacker on the shared host node either way.
  vcluster is a control-plane/API boundary, **not a compute boundary.**
- To add compute isolation, pair the vcluster tier with the **tainted/dedicated
  sandbox node pool** that already exists for `AgentSandbox` (dedicated nodes,
  PSS-`restricted`, and whatever runtime sandboxing — gVisor/Firecracker — the pool is
  configured with). That is an **orthogonal dial**, not something the virtual cluster
  provides. The next rung, a dedicated cluster
  (`tenant-isolation-tiers.md`), is the answer when even shared nodes are
  unacceptable.
- **Correction of record:** `SECURITY.md` currently calls the vcluster option
  "kernel-level boundaries" (line 22) and "kernel-level" hard isolation (line 55).
  That is wrong and oversells the control. The accurate claim is **API-server-level
  isolation**; kernel-level isolation requires the dedicated node pool or a dedicated
  cluster. This ADR is the source of truth the `SECURITY.md` isolation section is
  reconciled against when the tier's docs are made true.

### The syncer as a new trust boundary

The syncer (in the vcluster control-plane pod) holds a **host** ServiceAccount with
permission to create/update/delete pods, services, endpoints, secrets, configmaps,
and — with SA sync on — service accounts **in the tenant host namespace**. It is a new
privileged component, one per tenant.

| Threat                                                                        | STRIDE      | Mitigation                                                                                                                                                                                                                                                                                                                                                 |
| ----------------------------------------------------------------------------- | ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Compromised syncer manipulates host objects in the tenant namespace           | **T**/**E** | The syncer's host RBAC is **namespace-scoped to `tenants-<platform>`** — this ADR mandates **single-namespace mode**, not multi-namespace (which would grant the syncer cross-namespace host power). Blast radius is one tenant's own host namespace; it cannot touch other namespaces, cluster-scoped resources, or other tenants.                        |
| Compromised syncer creates privileged / quota-busting / open-egress host pods | **E**/**D** | The syncer's host creates are subject to the **same host admission and policy** as any pod in `tenants-<platform>`: PSS-`restricted` rejects privilege escalation, the `ResourceQuota` caps consumption, the default-deny `NetworkPolicy`/Cilium egress bounds reachability. A compromised syncer cannot exceed what the tenant namespace already permits. |
| Compromised syncer reaches AWS                                                | **E**       | The syncer's host SA carries **no Pod Identity association** and thus no IAM role — only the tenant `tenant-runtime` synced SA is bound to the per-Platform role. A compromised syncer has no AWS credential path.                                                                                                                                         |
| vcluster control-plane pod as a per-tenant DoS surface                        | **D**       | If it OOMs/crashes the blast radius is **one Platform** (its own tenant's API), not the fleet; the host `ResourceQuota` caps its resource use so it cannot starve neighbors. See Failure modes.                                                                                                                                                            |

The net: the syncer widens the per-tenant trust boundary by one component, but every
edge of that boundary is already fenced by the host containment controls the namespace
tier ships — so the syncer's blast radius is bounded by, and no larger than, the
tenant's own namespace.

## Failure modes

### vcluster control-plane pod down

Degrade loud, not silent. The target-client swap fails to build or reach the vcluster
client → the AgentFleet/AgentSandbox/etc. reconcile returns an error →
controller-runtime requeues with backoff. Host-side Platform reconciliation
(namespace, quota, netpol, IAM, KMS) is **independent** and keeps succeeding — the
containment layer stays intact even while the virtual cluster is down. The Platform
surfaces a new `VClusterReady=False` condition so the degradation is observable and
alertable (a `PrometheusRule` on that condition is wired when the tier is
implemented). Already-running synced pods on the host **keep running** — a
control-plane blip does not evict the data plane; only new reconciles into the
vcluster block until it recovers. A `docs/runbooks/vcluster-down.md` runbook documents
recovery and is authored with the implementation.

### Upgrade path

The vcluster version is a single knob: the pinned `spec.source.targetRevision` on the
operator-rendered Application, surfaced as a `vcluster.chartVersion` value on the
operator chart and rolled per-env like every other pinned chart version. Control-plane
upgrades are in-place (StatefulSet rolling update). Renovate proposes bumps; a human
reviews majors — the same chart-pin discipline ADR 0003's supply-chain section already
applies to kagent/agentgateway. The cross-version risk is specifically the synced-SA
naming algorithm (`SafeConcatName`), which is vcluster-internal and could change across
majors — which is exactly why the Pod Identity design discovers the synced SA by label
and cross-checks the computed name: an upgrade that changes naming fails observably
(the `discovered == computed` assertion trips) instead of silently breaking the
tenant's credential binding.

### Teardown ordering (finalizer semantics)

The existing discipline — add the finalizer **before** any provisioning
(`platform_controller.go:137-143`), tear down in reverse on delete
(`platform_controller.go:100-133`) — extends to cover the vcluster. On
`DeletionTimestamp`, before the finalizer drops, the operator must, in order:

1. Stop reconciling workload CRDs into the vcluster and delete the tenant app
   Applications targeting it.
2. Delete the ArgoCD **cluster Secret** that registered the vcluster destination.
3. Delete the vcluster **Application** — ArgoCD uninstalls the Helm release: the
   control-plane StatefulSet, the syncer, and the **synced host objects** it created
   (pods, services, the synced `tenant-runtime` SA).
4. Delete the **Pod Identity association** for the synced host SA. This is AWS-side
   state, not namespace-scoped, so it will **not** be reaped by namespace deletion and
   must be removed explicitly — exactly as the namespace-tier `tenant-runtime`
   association is (`deleteIamRole` → `deletePodIdentityAssociation`,
   `platform_iam.go:398`). The primitive is idempotent and tolerates a missing SA, so
   it is safe whether it runs before or after vcluster teardown has removed the SA.
5. Run the existing host cleanup: delete the tenant namespace (cascades quota, limit
   range, network policy, and any remaining synced pods), the `AppProject`, the IAM
   role, the KMS grant, and the bucket-policy statements
   (`cleanupTenantResources` + the AWS-side cleanups already in the finalizer flow).

The load-bearing invariant: **the finalizer must not drop until both the vcluster
instance and every host object it synced are gone.** The finalizer verifies the
vcluster Application is deleted and the host namespace holds no lingering
vcluster-managed objects (label `vcluster.loft.sh/managed-by=<vcluster-name>`) before
removing itself — otherwise a deleted Platform orphans a running virtual cluster (cost
and a live credential path) or leaves synced pods, a synced SA, and its Pod Identity
association behind. Each step tolerates NotFound/NoKindMatch so finalizer re-runs are
safe, matching the existing cleanup helpers.

## Version

Resolved from upstream at authoring time (2026-07-17):

- **vcluster** — latest stable **v0.35.2** (2026-07-09); `0.35.x` is the current
  stable minor line (`0.36.0-*` are pre-releases at authoring time).
- **Chart** — `vcluster`, chart version tracks the app version (chart `0.35.2`), from
  `https://charts.loft.sh` (OCI mirror `oci://ghcr.io/loft-sh/charts/vcluster`).
- **Source of the naming + annotation contract** — `pkg/util/translate` at tag
  `v0.35.1` (`SingleNamespaceHostName`, `SafeConcatName`, `types.go` constants).

Per the org version-currency rule, the implementation re-resolves the current
`0.35.x` chart version from the registry at the time it lands and pins that, rather
than inheriting a number from this ADR.

## Trade-offs

- **A new privileged per-tenant component.** The syncer holds host-namespace write
  credentials. Bounded to the tenant namespace by single-namespace-mode RBAC and the
  host containment controls, but it is real added surface — accounted for in the threat
  delta.
- **Per-tenant control-plane overhead.** Each vcluster-tier Platform runs a control-
  plane pod (and, on the recommended design, its own kagent + KEDA). That is the cost
  of the tier; the host `ResourceQuota` caps it, and only tenants that need hard
  isolation pay it.
- **ArgoCD becomes a hard dependency** for this tier (it is optional for the namespace
  tier). Accepted: the platform is already ArgoCD-delivered end to end.
- **A dependency on a vcluster-internal naming algorithm.** Mitigated by discovering
  the synced SA by label and treating the computed name as a cross-check rather than
  the source of truth — so an upstream change fails loud instead of silently.
- **Two-phase reconcile for the SA binding.** The association can only be created after
  the syncer has materialized the host SA, so the reconcile converges over more than
  one pass. The reconciler already tolerates multi-pass convergence.

## Alternatives considered

- **Helm-in-operator (SDK or subprocess) to install the vcluster.** Rejected — new
  heavy dependency / runtime surface, breaks the "operator never runs Helm" idiom
  (ADR 0008).
- **Operator hand-renders vcluster's manifests** into `charts/operator/files/`.
  Rejected — forces the operator to track vcluster's full, moving manifest surface.
- **Parallel per-tier reconcile paths** instead of a client swap. Rejected — duplicates
  every workload reconcile and drifts.
- **Recompute the synced SA name with the operator's `fnv1a64` idiom.** Rejected — the
  synced name is vcluster's to define (SHA-256, prefix 52, hash 10); the operator must
  discover it and, where it computes, replicate vcluster's algorithm exactly.
- **Operator applies tenant app charts into the vcluster directly**, bypassing ArgoCD
  cluster registration. Rejected — splits delivery and breaks the platform-tenant
  contract.
- **Multi-namespace vcluster mode.** Rejected — grants the syncer cross-namespace host
  RBAC, widening the syncer's blast radius past the tenant's own namespace. Single-
  namespace mode keeps the syncer fenced.
- **Sell namespace isolation as "hard" and drop the vcluster tier.** Rejected — the CRD
  already offers `vcluster` and the isolation-tiers model already promises API-level
  isolation at this rung; a tenant declaring `vcluster` must receive it, which is the
  reason this ADR exists.

## Cross-references

- CRD field: `operators/api/platform/v1alpha1/platform_types.go` (`Isolation`).
- Host-side reconcile the tier layers onto:
  `operators/internal/controller/platform_controller.go`,
  `operators/internal/controller/platform_reconcile.go`.
- Pod Identity primitives + the naming idiom this design parallels:
  `operators/internal/controller/platform_iam.go`
  (`ensurePodIdentityAssociation`, `deletePodIdentityAssociation`, `tenantRoleName`).
- Isolation model: `docs/architecture/tenant-isolation-tiers.md`.
- Threat model + cross-component contracts this ADR extends: [ADR 0003](0003-threat-model.md).
- Operator-renders-not-Helm precedent: [ADR 0008](0008-eval-runtime-operator-chart.md).
- Platform-tenant contract (delivery + Pod Identity + OTel attrs):
  `nanohype/standards/platform-tenant-contract.json`.
- Upstream: [loft-sh/vcluster](https://github.com/loft-sh/vcluster),
  [EKS Pod Identity integration](https://www.vcluster.com/docs/vcluster/third-party-integrations/pod-identity/eks-pod-identity),
  chart repo `https://charts.loft.sh`.
- Implementation of this design (reconcile path, chart surface, conformance + kx
  validation, docs made true) is tracked as the vcluster implementation work that
  follows this ADR.
