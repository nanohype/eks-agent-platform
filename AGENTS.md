# eks-agent-platform — agent entry point

You're an AI client (or the author of one) about to declare a tenant on an EKS cluster, ship an agent fleet, set a budget, or configure a model gateway. This file gets you running in five minutes. For the wider picture — how this repo fits into the nanohype stack — read the [Platform Reference](https://github.com/nanohype/nanohype/blob/main/docs/platform-reference.md).

## What this repo gives you

A Kubernetes-native control plane that lets you declare agent platforms as CRDs and have an operator reconcile the AWS state, namespace boundary, tenant IAM identity, KMS grants, network policies, and runtime resources. Nine CRDs (version `v1alpha1`) split across three capability groups under the `nanohype.dev` domain — `platform.nanohype.dev` (Tenant, Platform), `agents.nanohype.dev` (AgentFleet, ModelGateway, AgentSandbox, SandboxPool), `governance.nanohype.dev` (BudgetPolicy, EvalSuite, SLOPolicy):

| CRD            | What it owns                                                                                                                                                                                                 |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `Tenant`       | Cluster-scoped aggregate of a team's Platforms. Rolls up readiness, spend, and suspension state                                                                                                              |
| `Platform`     | Tenant Namespace, ResourceQuota, LimitRange, default-deny NetworkPolicy, ArgoCD AppProject, `tenant-runtime` ServiceAccount, per-Platform IAM role + Pod Identity association + KMS grant + S3 bucket policy |
| `ModelGateway` | Envoy AI Gateway routes, Bedrock model ID resolution, Guardrails attachment, per-route rate limits                                                                                                           |
| `AgentFleet`   | Deployment per agent running the tenant's own image, KEDA ScaledObject, per-fleet NetworkPolicy, all under the tenant ServiceAccount bound to the tenant IAM role via EKS Pod Identity                       |
| `SandboxPool`  | Pull-based pool of always-on Managed Agents sandbox workers — a worker Deployment, default-deny NetworkPolicy, and a KEDA-autoscaled metrics bridge keyed on work-queue depth                                |
| `AgentSandbox` | Single-use hardened pod for one agent role-session — push-dispatched, Platform-gated, default-deny networked, garbage-collected after a TTL                                                                  |
| `BudgetPolicy` | Hourly Athena rollup of CUR + CloudWatch in-flight estimate. Writes spend / percent / conditions to status. Publishes BudgetBreach to EventBridge at ≥120%                                                   |
| `EvalSuite`    | Argo CronWorkflow per suite. Gates Argo Rollouts via AnalysisTemplate on `status.lastScore`                                                                                                                  |
| `SLOPolicy`    | Multi-window burn-rate evaluation of one objective against Amazon Managed Prometheus. Publishes BurnRateBreach to EventBridge, and holds the tenant's ArgoCD auto-sync on a page-tier burn                   |

Plus:

- **`operators/`** — Go operator binary registering nine reconcilers (one binary, one leader-election lease)
- **`charts/`** — Helm charts for installing the operator (CRDs + Deployment + RBAC + the eval-runtime bundle behind `evalRuntime.*`) + the `tenant` chart consumers use. `eks-gitops` `addons-agent-operator` git-sources `charts/operator` and injects per-cluster IRSA to deliver it onto clusters
- **`examples/`** — minimal end-to-end CR sets (Tenant + Platform + ModelGateway + AgentFleet + BudgetPolicy) you can copy

## Contract surface

The Platform CR is the entry point. Minimum shape (full field reference in [`docs/crd-reference/v1alpha1.md`](docs/crd-reference/v1alpha1.md)):

```yaml
apiVersion: platform.nanohype.dev/v1alpha1
kind: Platform
metadata:
  name: my-app
  namespace: eks-agent-platform
spec:
  displayName: 'My App'
  persona: ops # sales-ops | support | finance | ops | founder | eng | marketing | legal | generic
  tenant: my-team
  budget:
    name: my-app # must reference an existing BudgetPolicy CR in the same namespace
  identity:
    allowedModelFamilies: [anthropic] # Bedrock families the operator-reconciled tenant role grants invoke on
    extraPolicyArns: [] # managed IAM policy ARNs to attach on top of the baseline
  compliance:
    soc2: true
  isolation: namespace # namespace (default) | vcluster
```

A `BudgetPolicy` CR is required (Platform.spec.budget.name references it):

```yaml
apiVersion: governance.nanohype.dev/v1alpha1
kind: BudgetPolicy
metadata:
  name: my-app
  namespace: eks-agent-platform
spec:
  platformRef: { name: my-app }
  monthlyUsd: '2500'
  alertThresholdsPercent: [50, 80, 100]
  killSwitchEnabled: true # at 120% the operator detaches the baseline IAM policy
```

### Tenant workload identity

A Platform's pods reach AWS through **one** IAM role, `<cluster>-<platform>-tenant`, and **one** ServiceAccount, `tenant-runtime`. The operator creates all three pieces of that binding:

- the tenant role, minted under the tenant IAM path with the configured tenant permissions boundary, trusting only `pods.eks.amazonaws.com`;
- the `tenant-runtime` ServiceAccount in the workload namespace `tenants-<platform>`, carrying no role-arn annotation. Under `spec.isolation: vcluster` the operator creates it inside the virtual cluster instead, and vcluster's syncer copies it to the host under a translated name;
- the **EKS Pod Identity association** binding that host ServiceAccount to the tenant role.

The operator creates no association for any other ServiceAccount, so a pod running under one gets no tenant credentials. Every workload that needs the tenant's AWS identity runs under `tenant-runtime`, and the [platform-tenant contract](https://github.com/nanohype/nanohype/blob/main/standards/platform-tenant-contract.json) holds a tenant chart to the same binding:

| Workload                                            | How it runs under `tenant-runtime`                                                                                                                                                               |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| The application's chart pods (`<app>/chart/`)       | The chart references the operator's ServiceAccount (`serviceAccount.create: false`, `serviceAccount.name: tenant-runtime`) and creates none of its own (contract rule `no-chart-serviceaccount`) |
| AgentFleet agent pods and AgentSandbox session pods | The operator sets `serviceAccountName: tenant-runtime` on the PodSpec it builds                                                                                                                  |
| The ModelGateway's Envoy proxy                      | The operator pins the EnvoyProxy's `envoyServiceAccount` to `tenant-runtime`, so the gateway invokes Bedrock as the tenant                                                                       |

No ServiceAccount a tenant chart renders or references carries an `eks.amazonaws.com/role-arn` annotation (contract rule `no-role-arn-annotation`): the annotation adds IRSA web-identity credentials to the pod, and the tenant role trusts only the Pod Identity service, never `sts:AssumeRoleWithWebIdentity`.

The tenant role carries the baseline Bedrock policy + `spec.identity.extraPolicyArns`, plus the inline policies the operator reconciles — model scoping, datastores, capabilities and direct secret reads from the Platform's declarations, and use of the tenant's own KMS key, whose ARN tenant-substrate publishes. Its name is cluster-keyed (not env-keyed) so two clusters can host a Platform of the same name without their roles colliding. The operator mints two further roles that no ServiceAccount is bound to: `<cluster>-<platform>-session` when `spec.attribution` is set, which only the tenant role may assume, and `<cluster>-<platform>-scheduler-invoke` when the `eventBridgeScheduler` capability is declared, which the tenant role passes to EventBridge Scheduler.

The association the operator created is reported on `Platform.status.podIdentity` — `clusterName`, `namespace`, `serviceAccount`, `roleArn`. Read it rather than deriving the binding: the trust policy carries no subject (under Pod Identity the `(namespace, service-account)` constraint lives in the association, so every tenant role's trust document is byte-identical), and the bound ServiceAccount is not always `tenant-runtime` — under `spec.isolation: vcluster` it is the vcluster-translated host name. Anything auditing the binding, in this repo or another, reads that field.

## Declare a tenant

1. Apply a `BudgetPolicy` CR in the tenant namespace (or alongside Platform — operator handles ordering).
2. Apply a `Platform` CR referencing the BudgetPolicy.
3. The operator reconciles:
   - Namespace `tenants-<platform>` (with `pod-security.kubernetes.io/enforce: restricted` label)
   - `ResourceQuota` + `LimitRange` defaults
   - Default-deny `NetworkPolicy` plus egress allow-list (DNS, the model gateway, OTel collector)
   - ArgoCD `AppProject` scoped to the tenant namespace
   - `tenant-runtime` ServiceAccount, the one the app chart and the operator-built pods run under
   - IAM role `<cluster>-<platform>-tenant` bound to the `tenant-runtime` SA via an EKS Pod Identity association; attaches baseline Bedrock policy + everything in `spec.identity.extraPolicyArns`, and reconciles a `bedrock-model-scoping` inline policy that limits Bedrock invoke to the ARNs `spec.identity.allowedModelFamilies` / `allowedModels` expand to (both unset = all model invoke denied)
4. Status reaches `Ready`; the app's ApplicationSet entry can start syncing.

## Ship an agent fleet

1. Confirm the tenant Platform is `Ready`.
2. Apply a `ModelGateway` CR (optional but recommended) declaring the model routes the agents will hit.
3. Apply an `AgentFleet` CR referencing the Platform. The operator reconciles a Deployment per agent — the tenant's own image, under the tenant ServiceAccount — plus the KEDA scaler.
4. Fleet pods run as the `tenant-runtime` SA; the Pod Identity association the operator created vends the tenant IAM role's credentials to them.

## Kill-switch

When a `BudgetPolicy` hits 120% of `monthlyUsd` and `killSwitchEnabled: true`, an EventBridge rule → Step Functions state machine:

1. Detaches the Bedrock-invoke baseline policy from the tenant's IAM role
2. Tags the role with `platform.nanohype.dev/suspended=true`
3. The `PlatformReconciler` observes the tag and stops re-attaching the baseline on subsequent reconciles
4. Status moves to `Suspended` with a `Suspended` condition

Recovery is **human-only** — an operator clears the suspension tag manually after the breach is resolved. The reconciler does not auto-restore.

## Conventions

- Conventional Commits enforced via `commitlint.config.mjs` (scope enum: `operators`, `charts`, `terraform`, `core`, `sdk`, `pricing`, `client`, `cli`, `examples`, `docs`, `ci`, `release`, `deps`, `security`)
- Go: `go fmt`, `go vet`, `golangci-lint` on PR
- Tests: `go test ./internal/...`; in-memory fakes for AWS clients (see `operators/internal/controller/platform_iam_reconcile_test.go` for the pattern)
- Generated artifacts (CRD manifests, deepcopy code) committed; `make manifests` regenerates them
- CRD API groups are org-aligned under the `nanohype.dev` domain: `platform.nanohype.dev` (Tenant, Platform), `agents.nanohype.dev` (AgentFleet, ModelGateway, AgentSandbox, SandboxPool), `governance.nanohype.dev` (BudgetPolicy, EvalSuite, SLOPolicy). Finalizers, label/tag keys, and the leader-election lease ID follow the same domain. A Platform's tenant field carries the owning team's real name — `strategy`, `growth`, `reliability`, `workplace`

## Pointers

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — bounded contexts, data flow, load-bearing decisions, the operator-owned-vs-tofu-owned split
- [`docs/crd-reference/v1alpha1.md`](docs/crd-reference/v1alpha1.md) — field-by-field CRD reference (generated from godoc)
- [`examples/`](examples/) — end-to-end CR sets you can copy
- [`README.md`](README.md) — install, run, contribute
- [Platform Reference](https://github.com/nanohype/nanohype/blob/main/docs/platform-reference.md) — the stack-wide view
- [`landing-zone/AGENTS.md`](../landing-zone/AGENTS.md) — its `agent-iam` component provisions the operator's own role, the tenant permissions boundary, and the IAM path every tenant role is minted under
