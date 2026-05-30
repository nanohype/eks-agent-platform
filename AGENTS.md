# eks-agent-platform — agent entry point

You're an AI client (or the author of one) about to declare a tenant on an EKS cluster, ship an agent fleet, set a budget, or configure a model gateway. This file gets you running in five minutes. For the wider picture — how this repo fits into the nanohype stack — read the [Platform Reference](https://github.com/nanohype/nanohype/blob/main/docs/platform-reference.md).

## What this repo gives you

A Kubernetes-native control plane that lets you declare agent platforms as CRDs and have an operator reconcile the AWS state, namespace boundary, IRSA, KMS grants, network policies, and runtime resources. Eight CRDs (version `v1alpha1`) split across three capability groups under the `nanohype.dev` domain — `platform.nanohype.dev` (Tenant, Platform), `agents.nanohype.dev` (AgentFleet, ModelGateway, AgentSandbox, SandboxPool), `governance.nanohype.dev` (BudgetPolicy, EvalSuite):

| CRD            | What it owns                                                                                                                                                                  |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Tenant`       | Cluster-scoped aggregate of a team's Platforms. Rolls up readiness, spend, and suspension state                                                                               |
| `Platform`     | Tenant Namespace, ResourceQuota, LimitRange, default-deny NetworkPolicy, ArgoCD AppProject, per-Platform IRSA role + KMS grant + S3 bucket policy                             |
| `ModelGateway` | agentgateway routes, Bedrock model ID resolution, Guardrails attachment, per-route rate limits                                                                                |
| `AgentFleet`   | kagent Agent + ModelConfig per agent, KEDA ScaledObject, per-fleet NetworkPolicy, tenant ServiceAccount with IRSA, optional DRA AcceleratorClaim                              |
| `SandboxPool`  | Pull-based pool of always-on Managed Agents sandbox workers — a worker Deployment, default-deny NetworkPolicy, and a KEDA-autoscaled metrics bridge keyed on work-queue depth |
| `AgentSandbox` | Single-use hardened pod for one agent role-session — push-dispatched, Platform-gated, default-deny networked, garbage-collected after a TTL                                   |
| `BudgetPolicy` | Hourly Athena rollup of CUR + CloudWatch in-flight estimate. Writes spend / percent / conditions to status. Publishes BudgetBreach to EventBridge at ≥120%                    |
| `EvalSuite`    | Argo CronWorkflow per suite. Gates Argo Rollouts via AnalysisTemplate on `status.lastScore`                                                                                   |

Plus:

- **`operators/`** — Go operator binary registering eight reconcilers (one binary, one leader-election lease)
- **`charts/`** — Helm charts for installing the operator + the `tenant` chart consumers use
- **`gitops/`** — ApplicationSets the gitops repos reference to install the operator
- **`examples/`** — minimal end-to-end CR sets (Tenant + Platform + ModelGateway + AgentFleet + BudgetPolicy) you can copy

## Contract surface

The Platform CR is the entry point. Minimum shape (full field reference in [`docs/crd-reference/v1alpha1.md`](docs/crd-reference/v1alpha1.md)):

```yaml
apiVersion: platform.nanohype.dev/v1alpha1
kind: Platform
metadata:
  name: my-app
  namespace: tenants-my-team
spec:
  displayName: 'My App'
  persona: ops # sales-ops | support | finance | ops | founder | eng | marketing | legal | generic
  tenant: my-team
  budget:
    name: my-app # must reference an existing BudgetPolicy CR in the same namespace
  identity:
    allowedModelFamilies: [anthropic] # Bedrock families the operator-reconciled IRSA role grants invoke on
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
  namespace: tenants-my-team
spec:
  platformRef: { name: my-app }
  monthlyUsd: '2500'
  alertThresholdsPercent: [50, 80, 100]
  killSwitchEnabled: true # at 120% the operator detaches the baseline IAM policy
```

### The two-role IRSA picture

A Platform tenant ends up with **two** IRSA roles serving different workload classes. This is intentional, not duplication:

| Role                   | Owner                                        | Trust subject                                         | Used by                                                           |
| ---------------------- | -------------------------------------------- | ----------------------------------------------------- | ----------------------------------------------------------------- |
| `<env>-<app>-platform` | `landing-zone/components/aws/<app>-platform` | `system:serviceaccount:tenants-<team>:<app>`          | The application's chart pods (the one shipped via `<app>/chart/`) |
| `<env>-<app>-tenant`   | This operator                                | `system:serviceaccount:tenants-<team>:tenant-runtime` | AgentFleet pods landing in this Platform's namespace              |

The app's chart annotates its SA with the landing-zone role's ARN (via `aws.platformRoleArn` Helm value). AgentFleet pods use the operator-created role with the baseline + `extraPolicyArns`.

## Declare a tenant

1. Apply a `BudgetPolicy` CR in the tenant namespace (or alongside Platform — operator handles ordering).
2. Apply a `Platform` CR referencing the BudgetPolicy.
3. The operator reconciles:
   - Namespace `tenants-<team>` (with `pod-security.kubernetes.io/enforce: restricted` label)
   - `ResourceQuota` + `LimitRange` defaults
   - Default-deny `NetworkPolicy` plus egress allow-list (DNS, agentgateway, OTel collector)
   - ArgoCD `AppProject` scoped to the tenant namespace
   - IRSA role `<env>-<app>-tenant` with trust policy targeting `tenant-runtime` SA; attaches baseline Bedrock policy + everything in `spec.identity.extraPolicyArns`
4. Status reaches `Ready`; the app's ApplicationSet entry can start syncing.

## Ship an agent fleet

1. Confirm the tenant Platform is `Ready`.
2. Apply a `ModelGateway` CR (optional but recommended) declaring the model routes the agents will hit.
3. Apply an `AgentFleet` CR referencing the Platform. The operator reconciles kagent `Agent` + `ModelConfig` + `ToolServer` resources plus the KEDA scaler.
4. Fleet pods run as `tenant-runtime` SA, picking up the operator-created IRSA role.

## Kill-switch

When a `BudgetPolicy` hits 120% of `monthlyUsd` and `killSwitchEnabled: true`, an EventBridge rule → Step Functions state machine:

1. Detaches the Bedrock-invoke baseline policy from the tenant's IRSA role
2. Tags the role with `platform.nanohype.dev/suspended=true`
3. The `PlatformReconciler` observes the tag and stops re-attaching the baseline on subsequent reconciles
4. Status moves to `Suspended` with a `Suspended` condition

Recovery is **human-only** — an operator clears the suspension tag manually after the breach is resolved. The reconciler does not auto-restore.

## Conventions

- Conventional Commits enforced via `commitlint.config.mjs` (scope enum: `operators`, `charts`, `gitops`, `terraform`, `core`, `sdk`, `pricing`, `client`, `cli`, `examples`, `docs`, `ci`, `release`, `deps`, `security`)
- Go: `go fmt`, `go vet`, `golangci-lint` on PR
- Tests: `go test ./internal/...`; in-memory fakes for AWS clients (see `operators/internal/controller/platform_iam_reconcile_test.go` for the pattern)
- Generated artifacts (CRD manifests, deepcopy code) committed; `make manifests` regenerates them
- CRD API groups are org-aligned under the `nanohype.dev` domain: `platform.nanohype.dev` (Tenant, Platform), `agents.nanohype.dev` (AgentFleet, ModelGateway, AgentSandbox, SandboxPool), `governance.nanohype.dev` (BudgetPolicy, EvalSuite). Finalizers, label/tag keys, and the leader-election lease ID follow the same domain. The tenant team identifier stays `protohype`

## Pointers

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — bounded contexts, data flow, load-bearing decisions, the operator-owned-vs-tofu-owned split
- [`docs/crd-reference/v1alpha1.md`](docs/crd-reference/v1alpha1.md) — field-by-field CRD reference (generated from godoc)
- [`examples/`](examples/) — end-to-end CR sets you can copy
- [`README.md`](README.md) — install, run, contribute
- [Platform Reference](https://github.com/nanohype/nanohype/blob/main/docs/platform-reference.md) — the stack-wide view
- [`landing-zone/AGENTS.md`](../landing-zone/AGENTS.md) — provisions the `<app>-platform` substrate the chart's IRSA role lives in
