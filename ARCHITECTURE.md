# Architecture

`eks-agent-platform` is a Kubernetes-native control plane for hosting agent platforms as declarative tenants on AWS EKS. This document covers the bounded contexts, the CRD surface, the AWS side, the data flow, and the load-bearing decisions.

## Bounded contexts

The system organizes around nine bounded contexts. Each gets a CRD, a reconciler in the operator binary, and (where it makes sense) an OpenTofu component and a Helm chart.

| Context           | CRD            | Reconciler | OpenTofu component             | Helm chart       | What it owns                                                                                                                                                                                                                            |
| ----------------- | -------------- | ---------- | ------------------------------ | ---------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Tenancy**       | `Tenant`       | `tenant`   | —                              | `tenant`         | Cluster-scoped aggregate of a team's `Platform`s; rolls up readiness, spend, and suspension into a single dashboard surface                                                                                                             |
| **Workspace**     | `Platform`     | `platform` | —                              | `tenant`         | Tenant `Namespace` (with Pod Security Standards label), `ResourceQuota`, `LimitRange`, default-deny `NetworkPolicy`, ArgoCD `AppProject`, per-Platform IRSA role + KMS grant + S3 bucket policy                                         |
| **Model access**  | `ModelGateway` | `gateway`  | `bedrock`, `agent-egress`      | `bedrock-egress` | agentgateway `Route` per `ModelRoute`, Bedrock model ID resolution, Bedrock Guardrails attachment, per-route rate limits                                                                                                                |
| **Agent runtime** | `AgentFleet`   | `runtime`  | `accelerator-pools`            | —                | kagent `Agent` + `ModelConfig` per agent, KEDA `ScaledObject` (SQS depth or CPU), per-fleet `NetworkPolicy`, tenant `ServiceAccount` bound to the tenant IAM role via EKS Pod Identity                                                  |
| **Budgets**       | `BudgetPolicy` | `budget`   | `cost-pipeline`, `kill-switch` | —                | Hourly Athena rollup of the CUR table + CloudWatch in-flight estimate; writes spend/percent/conditions to `BudgetPolicy.status`; publishes `BudgetBreach` to EventBridge at ≥120%                                                       |
| **Evals**         | `EvalSuite`    | `eval`     | `model-artifacts`              | `operator`       | Argo `CronWorkflow` per suite referencing the `eval-runner` `WorkflowTemplate` (shipped by the operator chart behind `evalRuntime.*`); status writeback by the runner; gates Argo Rollouts via `AnalysisTemplate` on `status.lastScore` |
| **Observability** | —              | —          | —                              | —                | OTel pipeline (from `eks-gitops`) carries `agents.tenant`, `agents.platform`, `agents.model_family`, `agents.model_id` resource attrs; Bedrock invocation spans + per-invocation cost                                                   |

The CRDs are split across three capability groups under the `nanohype.dev` domain, all at version `v1alpha1`:

- **`platform.nanohype.dev`** — the Tenancy and Workspace contexts: `Tenant`, `Platform`
- **`agents.nanohype.dev`** — the Model-access and Agent-runtime contexts plus the sandbox kinds: `AgentFleet`, `ModelGateway`, `AgentSandbox`, `SandboxPool`, `BatchJob`
- **`governance.nanohype.dev`** — the Budgets and Evals contexts: `BudgetPolicy`, `EvalSuite`

The field-level reference is regenerated from godoc on every `make manifests` into [`docs/crd-reference/v1alpha1.md`](./docs/crd-reference/v1alpha1.md).

## Key architectural decisions

### One operator binary, nine reconcilers

A single Go binary registers nine reconcilers (`tenant`, `platform`, `gateway`, `runtime`, `budget`, `eval`, `sandboxpool`, `agentsandbox`, `batch`) with one shared leader-election lease. Operationally simpler than six deployments; the split is trivial if any reconciler outgrows it.

### Operator owns fast-moving AWS state; OpenTofu owns slow-moving infra

Per-tenant IRSA roles, KMS grants, and Bedrock model-access policies are reconciled by the operator via the AWS SDK (the operator pod runs with an IRSA role that grants it `iam:*` on a constrained path, `kms:CreateGrant` on the data CMK, etc.). Putting per-tenant resources in OpenTofu means a `Platform` CR apply triggers a Terragrunt deploy — minutes of latency, brittle, doesn't fit a reconcile loop. Karpenter, ACK, and the EKS Pod Identity Agent all use this pattern.

OpenTofu owns: invocation logging buckets, base IAM, EventBridge bus, cost pipeline, Bedrock Guardrails templates, VPC endpoints, WAF — the slow-moving substrate.

### Wrap kagent, don't fork it

`AgentFleet` reconciles into upstream kagent `Agent` + `ModelConfig` + `ToolServer` CRs plus the platform-specific scaffolding (IRSA binding, KEDA scaler, NetworkPolicy, OTel attrs, BudgetPolicy reference). When kagent ships a new feature it's available immediately; when our composite adds value, that value is concentrated in the operator.

Same with agentgateway: `ModelGateway` reconciles into upstream agentgateway `Route` + `Listener` resources.

### Accelerator node substrate

`terraform/components/accelerator-pools` provisions the substrate for GPU/Neuron workloads: IRSA + EKS Pod Identity for the NVIDIA GPU Operator and the AWS Neuron device plugin, plus an SSM `pool_catalog` parameter enumerating the available pools by device class (`gpu.nvidia.com`, `neuron.aws.com`) and instance family (NVIDIA on `g6e`/`p5`, Neuron on `inf2`/`trn2`). The GPU operator, the NVIDIA DRA driver, and the AWS Neuron device plugin are installed by the eks-gitops accelerators addon group. Fleet-level scheduling onto these pools is out of scope for v1 — see [What this repo deliberately does NOT do](#what-this-repo-deliberately-does-not-do).

### The operator carries its own runtime

The operator chart (`charts/operator`) ships more than the controller and CRDs. Two of its own runtime pieces ride along behind values toggles:

- **`evalRuntime.*`** — the eval-runtime: the `eval-runner` Argo `WorkflowTemplate`, the `AnalysisTemplate` that gates Rollouts on `status.lastScore`, and the `ServiceAccount` + RBAC the runner needs. Source under `charts/operator/{files,templates}/eval-runtime/`. The eval-runner role ARN and the report bucket are injected per-cluster by the eks-gitops addon that deploys the operator.
- **`slo.*`** — the operator SLO: a `PrometheusRule`, an `AlertmanagerConfig`, and the kube-state-metrics CR-state config that exposes the CRDs as metrics. Source under `charts/operator/{files,templates}/slo/`.

Keeping these in the chart means the operator's eval gating and its own SLO arrive with the operator instead of being a separate install step.

### Bedrock-only model plane in v1

`@eks-agent/sdk` ships a `BedrockAdapter` base with a family registry (`packages/sdk/src/factory.ts`); two family adapters are registered — Anthropic and Amazon Nova — each with uniform call shape, family-accurate pricing, family-accurate error taxonomy. Adding a Bedrock family is a `BedrockAdapter` subclass plus a registry insert; adding a non-Bedrock provider later is a new `ProviderAdapter` implementation, not an architecture change.

### Two CMKs per cluster, isolated by grant

Each cluster carries two customer-managed keys, provisioned once by landing-zone — not one pair per Platform. Tenant isolation is a scoped grant, not a dedicated key.

- **`cmk-data`** encrypts the model-artifact bucket, the audit S3 bucket, and the EventBridge archive. The operator issues each tenant role a KMS grant on this key constrained to `EncryptionContext={PlatformId: <platform>}`, so tenant A's role cannot decrypt tenant B's objects — B's data is encrypted under a context A was never granted. The auditor role has **no** decrypt permission.
- **`cmk-logs`** encrypts CloudWatch log groups and the Bedrock invocation logging bucket. Auditor role has decrypt **only on this key**.

A breach of the auditor role surfaces audit history (an acceptable disclosure for oversight) but does not unlock data-plane content, and a breach of one tenant role reaches only that tenant's encryption context.

### Kill-switch is human-recovery only

A `BudgetPolicy` breach at ≥120% triggers an EventBridge rule → Step Functions state machine that:

1. Detaches the Bedrock-invoke baseline policy from the tenant's IRSA role.
2. Tags the role with `platform.nanohype.dev/suspended=true` so the `PlatformReconciler` won't re-attach the baseline on its next tick.

The operator detects the tag on its next reconcile (≤60s in production), sets `Platform.status.phase = Suspended`, and the `AgentFleetReconciler` tears down the fleet's kagent `Agent`s and KEDA `ScaledObject` — pods scale to zero and stop serving traffic. Recovery is exclusively human: ops removes the IAM tag (typically via an SSO elevation flow with MFA + approver), and the next reconcile reattaches the baseline and scales the fleet back up. No CR mutation, no API path back.

### Observability

Every signal flows through the OTel Collector already installed by `eks-gitops`. This repo adds a `eks-agent-platform` pipeline:

```
agent pod → OTLP (localhost:4317) → OTel Collector
   → memory_limiter
   → resource processor (adds tenant, platform, workspace, model_family, model_id)
   → transform processor (PII redaction on log bodies)
   → batch
   → exporters: awscloudwatch (always) + datadog (optional, gated on values)
```

#### Resource-attribute coverage

The platform-tenant contract requires `agents.tenant` and `agents.platform` (plus `agents.model_family` / `agents.model_id` for AI workloads) on every pod. The operator honors this on the pods it builds itself — AgentSandbox session pods, SandboxPool workers, and the eval-runner workflow step all get `OTEL_RESOURCE_ATTRIBUTES` stamped from the owning Platform (`operators/internal/controller/otel.go`), with `agents.model_family` added when the Platform pins exactly one family.

AgentFleet and ModelGateway runtime pods are a deliberate exception, and it is a real limitation rather than a gap papered over. The operator emits a kagent `Agent` (+ `ModelConfig`) and agentgateway `Gateway` / `HTTPRoute` / `Backend` CR; kagent and agentgateway render the actual Deployments and pods. The operator never builds those PodSpecs, and neither the kagent `Agent` v1alpha2 declarative schema nor the agentgateway backend schema exposes an env or pod-template passthrough, so the env-var mechanism cannot reach them. What the operator does stamp is `agents.platform` / `agents.fleet` / `agents.agent` labels on the CRs it emits — the hook a collector-side attribution processor keys on — so these pods are attributable by label, just not by the env-var self-report. Closing the gap fully is upstream work on the kagent / agentgateway CRDs (a pod-template or resource-attribute field); the platform does not reintroduce a mutating webhook to force it.

Per-persona Grafana dashboards live in `eks-gitops` (`dashboards/`, rendered by the grafana-operator as `GrafanaDashboard` CRs):

- **Finance** — spend by tenant, top-N models, forecast vs. budget
- **Ops** — queue depth, eval scores, error budgets, model latency p50/p95/p99
- **Founder/Exec** — tenants live, weekly spend trend, top initiatives by agent activity

## Data flow: a single agent invocation

```
1. App pod (tenant) builds a Messages request via @eks-agent/sdk
2. SDK signs request with tenant IRSA → POSTs to agentgateway via cluster service
3. agentgateway resolves the ModelRoute named on AgentFleet → Bedrock model ID
4. Bedrock Guardrails attached at the route level run input policy
5. agentgateway issues bedrock-runtime InvokeModel via PrivateLink VPC endpoint
6. Bedrock response flows back through Guardrails output policy
7. agentgateway emits OTel span with cost attrs (input/output tokens × pricing)
8. SDK in app pod emits OTel span with correlation_id linking the request
9. Collector exports to CloudWatch + (optional) Datadog
10. invocation-cost-publisher Lambda tails the Bedrock invocation log group, emits
    EstimatedInvocationCostUsd to CloudWatch with PlatformId dimension
11. BudgetReconciler ticks hourly: SUMs current month CUR via Athena + adds the
    last-24h CloudWatch in-flight; writes spend/percent to BudgetPolicy.status
12. At ≥120% with killSwitchEnabled, the reconciler PutEvents'es BudgetBreach to
    the kill-switch EventBridge bus → SFN detaches policy + tags role suspended
```

## Repository layout

See [README.md](./README.md#what-you-get).

## What this repo deliberately does NOT do

- **Not a model host.** Bedrock runs inference outside the cluster. This platform does not change that. Self-hosted models on Neuron/NVIDIA are out of scope for v1: the accelerator node substrate is provisioned (see [Accelerator node substrate](#accelerator-node-substrate)), but fleet-level accelerator scheduling — the AgentFleet API to request a device class and the reconcile path onto these pools — is tracked in [#106](https://github.com/nanohype/eks-agent-platform/issues/106).
- **Not multi-cloud.** EKS only.
- **Not a replacement for kagent or agentgateway.** It composes them.
- **Not a cluster bootstrap.** The cluster + ArgoCD must already exist (via `landing-zone` OpenTofu or equivalent).
