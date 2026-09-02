# Architecture

`eks-agent-platform` is a Kubernetes-native control plane for hosting agent platforms as declarative tenants on AWS EKS. This document covers the bounded contexts, the CRD surface, the AWS side, the data flow, and the load-bearing decisions.

## Bounded contexts

The system organizes around nine bounded contexts. The eight CRD-backed ones each get their CRDs and reconcilers in the operator binary, and (where it makes sense) an OpenTofu component and a Helm chart.

| Context           | CRD            | Reconciler | OpenTofu component             | Helm chart       | What it owns                                                                                                                                                                                                                            |
| ----------------- | -------------- | ---------- | ------------------------------ | ---------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Tenancy**       | `Tenant`       | `tenant`   | —                              | `tenant`         | Cluster-scoped aggregate of a team's `Platform`s; rolls up readiness, spend, and suspension into a single dashboard surface                                                                                                             |
| **Workspace**     | `Platform`     | `platform` | —                              | `tenant`         | Tenant `Namespace` (with Pod Security Standards label), `ResourceQuota`, `LimitRange`, default-deny `NetworkPolicy`, ArgoCD `AppProject`, per-Platform IAM role + Pod Identity association + KMS grant + S3 bucket policy                                         |
| **Model access**  | `ModelGateway` | `gateway`  | `bedrock`, `agent-egress`      | `bedrock-egress` | Envoy AI Gateway `AIGatewayRoute` rule per `ModelRoute`, Bedrock model ID resolution, Bedrock Guardrails attached as request headers, per-route rate limits                                                                             |
| **Agent runtime** | `AgentFleet`   | `runtime`  | —                              | —                | `Deployment` per agent running the tenant's own image, KEDA `ScaledObject` (SQS depth or CPU), per-fleet `NetworkPolicy`, all under the tenant `ServiceAccount` bound to the tenant IAM role via EKS Pod Identity                       |
| **Sandboxing**    | `SandboxPool`, `AgentSandbox` | `sandbox`, `agentsandbox` | —          | `operator`       | Platform-scoped pool of self-hosted sandbox workers plus single-use per-session pods; both run Pod Security `restricted` and default-deny networked on the dedicated tainted sandbox node pool                        |
| **Budgets**       | `BudgetPolicy` | `budget`   | `cost-pipeline`, `kill-switch` | —                | Hourly Athena rollup of the CUR table + CloudWatch in-flight estimate; writes spend/percent/conditions to `BudgetPolicy.status`; publishes `BudgetBreach` to EventBridge at ≥120%                                                       |
| **Evals**         | `EvalSuite`    | `eval`     | `eval-runtime`                 | `operator`       | Argo `CronWorkflow` per suite referencing the `eval-runner` `WorkflowTemplate` (shipped by the operator chart behind `evalRuntime.*`); status writeback by the runner; gates Argo Rollouts via `AnalysisTemplate` on `status.lastScore` |
| **Reliability**   | `SLOPolicy`    | `slo`      | `kill-switch`                  | `operator`       | Burn-rate evaluation over the Platform's SLI; a page-tier error-budget burn emits on the kill-switch EventBridge bus and holds the tenant's rollout                                                                    |
| **Observability** | —              | —          | —                              | —                | OTel pipeline (from `eks-gitops`) carries `agents.tenant`, `agents.platform`, `agents.model_family` resource attrs (model id rides on the per-invocation span, not the pod resource); Bedrock invocation spans + per-invocation cost    |

The CRDs are split across three capability groups under the `nanohype.dev` domain, all at version `v1alpha1`:

- **`platform.nanohype.dev`** — the Tenancy and Workspace contexts: `Tenant`, `Platform`
- **`agents.nanohype.dev`** — the Model-access and Agent-runtime contexts plus the sandbox kinds: `AgentFleet`, `ModelGateway`, `AgentSandbox`, `SandboxPool`
- **`governance.nanohype.dev`** — the Budgets, Evals and Reliability contexts: `BudgetPolicy`, `EvalSuite`, `SLOPolicy`

The field-level reference is regenerated from godoc on every `make manifests` into [`docs/crd-reference/v1alpha1.md`](./docs/crd-reference/v1alpha1.md).

## Key architectural decisions

### One operator binary, nine reconcilers

A single Go binary registers nine reconcilers (`tenant`, `platform`, `gateway`, `runtime`, `sandbox`, `agentsandbox`, `budget`, `slo`, `eval`) with one shared leader-election lease. Operationally simpler than nine deployments; the split is trivial if any reconciler outgrows it.

### Operator owns fast-moving AWS state; OpenTofu owns slow-moving infra

Per-tenant IAM roles, their Pod Identity associations, KMS grants, and Bedrock model-access policies are reconciled by the operator via the AWS SDK (the operator pod itself runs with an IRSA role that grants it `iam:*` on a constrained path, `kms:CreateGrant` on the data CMK, etc.). Putting per-tenant resources in OpenTofu means a `Platform` CR apply triggers a Terragrunt deploy — minutes of latency, brittle, doesn't fit a reconcile loop. Karpenter, ACK, and the EKS Pod Identity Agent all use this pattern.

OpenTofu owns: invocation logging buckets, base IAM, EventBridge bus, cost pipeline, Bedrock Guardrails templates, VPC endpoints, WAF — the slow-moving substrate.

### Own the agent's PodSpec, not its framework

`AgentFleet` reconciles into a Deployment per agent — the tenant's own image, under the tenant ServiceAccount — plus the platform scaffolding (KEDA scaler, NetworkPolicy, OTel attrs, BudgetPolicy reference). The platform supplies no agent framework and no tool server: the agent loop and its tools are the tenant's code, running in the tenant's process.

That is an attribution decision before it is a packaging one. A tool server executes an agent's actions under its own service identity, so the audit log names the tool server rather than the agent that asked — and an agent's account of what it did cannot then be confirmed or refuted against the record. Tools in-process, as the tenant, make the two records name the same principal.

Same with Envoy AI Gateway: `ModelGateway` reconciles into a Gateway-API `Gateway` plus upstream `AIGatewayRoute` / `AIServiceBackend` / `BackendSecurityPolicy` resources.

### The operator carries its own runtime

The operator chart (`charts/operator`) ships more than the controller and CRDs. One of its own runtime pieces rides along behind a values toggle:

- **`evalRuntime.*`** — the eval-runtime: the `eval-runner` Argo `WorkflowTemplate`, the `AnalysisTemplate` that gates Rollouts on `status.lastScore`, and the `ServiceAccount` + RBAC the runner needs. Source under `charts/operator/{files,templates}/eval-runtime/`. The eval-runner role ARN and the report bucket are injected per-cluster by the eks-gitops addon that deploys the operator.

Keeping it in the chart means the operator's eval gating arrives with the operator instead of being a separate install step.

The operator's own SLO alerting is not in this chart. `values.schema.json` closes the top level, so `--set slo.enabled=true` fails with `additional properties 'slo' not allowed` rather than being quietly ignored. Burn-rate evaluation is the `slo` reconciler's control loop — toggled by `reconcilers.slo` — writing its verdict to `SLOPolicy.status`, which the eks-gitops kube-state-metrics addon projects as metrics; the paging rules are Grafana-managed against AMP in `eks-gitops/dashboards/base/alerting/agent-operator.yaml`. That catalog installs `prometheus-operator-crds` and no ruler, so a chart-shipped `PrometheusRule` would be applied to every cluster and evaluated by none.

### The gateway is the model plane

There is no in-process provider adapter. Every model call leaves a tenant as ordinary HTTP to that Platform's `ModelGateway`, and the gateway holds the AWS identity, applies the route's guardrail and rate limit, and records the request. An application that reached a model any other way would have none of the three, which is why the boundary is a network hop rather than a library: `gatewayEgressCiliumRules` gives outbound TLS to the gateway's Envoy pods alone, so every other pod in the namespace is left without a route to a model. The gateway is enforced, not merely offered.

What an application holds is a route *name* and a base URL. The `ModelGateway` CR maps each route to a Bedrock model — a foundation model, an inference profile, or an open-weight model imported through Custom Model Import — so repointing a route at a different model is a CR edit and the application is untouched. `spec.routes[].api` fixes the wire format across such a change, and the resolved format and its base URL are published on `status.routes[]` for clients to read rather than assume.

Model families therefore are not a code concern. Adding one is a route on a CR; the only thing the repo tracks per family is pricing (`@eks-agent/pricing`), which the cost path needs whatever the wire format was.

### One cluster CMK by default, a second only where the readers differ

A cluster carries **one** customer-managed key by default, provisioned once by landing-zone's `secrets` component — not one per Platform. Both `data_kms_key_arn` and `logs_kms_key_arn` resolve to it. Setting `separate_logs_key` on that component mints a second CMK and **moves** the CloudWatch Logs and Bedrock grants onto it, which is what makes "reads logs, cannot decrypt data" a boundary rather than a sentence. It is off by default: the second key is worth its rotation, audit and cost only where the log reader and the data reader are different people.

- **The platform key** encrypts the model-artifacts bucket, the Athena/estimates buckets, and the EventBridge archive; it also encrypts CloudWatch log groups and the Bedrock invocation-logging bucket unless the log path has been separated.
- **Each tenant's own key** is separate and real: `tenant-substrate` mints one per Platform, and the operator grants the tenant role `GenerateDataKey`/`Decrypt`/`DescribeKey` on exactly that ARN. Resource-scoped to one key, so tenant A's policy names an ARN that is not tenant B's.

**Tenant isolation on the shared model-artifacts bucket is an IAM boundary, not a cryptographic one.** The operator writes a bucket-policy statement per Platform scoped to `tenants/<platform>/*`, and that is the entire separation — nothing else grants a tenant role object access to that bucket, so a cross-tenant read is an implicit deny at S3 before KMS is reached. The KMS layer cannot discriminate between tenants there: the baseline policy gives every tenant role the same `kms:Decrypt` on the platform key (conditioned only on `kms:ViaService = s3`), and S3's SSE-KMS encryption context is `aws:s3:arn` — the *bucket* ARN, since the bucket enables an S3 Bucket Key — which is byte-identical for every tenant's objects. Anything that loosens the prefix policy removes the only control, and there is no second one behind it.

### Kill-switch is human-recovery only

A `BudgetPolicy` breach at ≥120% triggers an EventBridge rule → Step Functions state machine that:

1. Detaches the Bedrock-invoke baseline policy from the tenant's IAM role.
2. Tags the role with `platform.nanohype.dev/suspended=true` so the `PlatformReconciler` won't re-attach the baseline on its next tick.

The operator detects the tag on its next reconcile (≤60s in production), sets `Platform.status.phase = Suspended`, and the `AgentFleetReconciler` tears down the fleet's agent `Deployment`s and KEDA `ScaledObject` — pods scale to zero and stop serving traffic. Recovery is exclusively human: ops removes the IAM tag (typically via an SSO elevation flow with MFA + approver), and the next reconcile reattaches the baseline and scales the fleet back up. No CR mutation, no API path back.

### Observability

Every signal flows through the OTel Collector already installed by `eks-gitops`. This repo adds a `eks-agent-platform` pipeline:

```
agent pod → OTLP (telemetry.monitoring.svc.cluster.local:4317) → OTel Collector
   → memory_limiter
   → resource processor (adds tenant, platform, workspace, model_family)
   → transform processor (PII redaction on log bodies)
   → batch
   → exporters: awscloudwatch (always) + datadog (optional, gated on values)
```

#### Resource-attribute coverage

The platform-tenant contract requires `agents.tenant` and `agents.platform` (plus `agents.model_family` / `agents.model_id` for AI workloads) on every pod. The operator honors this on the pods it builds itself. AgentSandbox session pods and SandboxPool workers get `OTEL_RESOURCE_ATTRIBUTES` stamped directly onto the PodSpec from the owning Platform (`operators/internal/controller/otel.go`), with `agents.model_family` added when the Platform pins exactly one family. The eval-runner workflow step is not an operator-built PodSpec — it runs from the eval-runtime WorkflowTemplate (`charts/operator/files/eval-runtime/workflow-template.yaml`), whose `run-cases` container sets `agents.tenant` / `agents.platform` from workflow parameters the operator fills in from the owning Platform (`operators/internal/controller/eval_reconcile.go`). That step does not carry `agents.model_family`: an eval run drives the fleet's gateway rather than a single pinned model family.

AgentFleet pods carry the attributes directly: the operator builds their PodSpec, so `agents.tenant` / `agents.platform` / `agents.fleet` / `agents.agent` are set as resource attributes on the container. That matters more than tidiness — the agent SDK reports its own agent id, which defaults to a constant, so without these every agent in a fleet emits indistinguishable spans and a claim cannot be matched to the record of what happened.

The ModelGateway's Envoy is the remaining exception, and a small one. Envoy Gateway renders that pod from the `EnvoyProxy` the operator emits; the operator stamps `agents.platform` on the CR, so the pod is attributable by label rather than by self-report. `EnvoyProxy` does expose a pod template, so closing it fully is available if the label path ever proves insufficient.

Per-persona Grafana dashboards live in `eks-gitops` (`dashboards/`, rendered by the grafana-operator as `GrafanaDashboard` CRs):

- **Finance** — spend by tenant, top-N models, forecast vs. budget
- **Ops** — queue depth, eval scores, error budgets, model latency p50/p95/p99
- **Founder/Exec** — tenants live, weekly spend trend, top initiatives by agent activity

## Data flow: a single agent invocation

```
1. App pod (tenant) builds an Anthropic Messages request
2. Request goes to the Platform's gateway Service in the tenant namespace — the
   app holds no AWS credential and signs nothing
3. The AIGatewayRoute matches the route name and rewrites it to the Bedrock model
   ID via modelNameOverride
4. Bedrock Guardrails attached as request headers run input policy
5. Envoy, running under the tenant ServiceAccount and its Pod Identity
   association, signs and issues bedrock-runtime InvokeModel via the PrivateLink
   VPC endpoint
6. Bedrock response flows back through Guardrails output policy
7. The gateway emits OTel spans with cost attrs (input/output tokens × pricing)
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

- **Not a model host.** Bedrock runs inference outside the cluster, including open-weight models through Custom Model Import. There is no accelerator substrate here and no GPU or Neuron node story: a fleet is a `Deployment` of the tenant's own image, and the model call leaves the cluster.
- **Not multi-cloud.** EKS only.
- **Not a replacement for Envoy AI Gateway.** It composes it. Nor an agent framework — the agent loop is the tenant's own code, and the platform's job is the boundary around it.
- **Not a cluster bootstrap.** The cluster + ArgoCD must already exist (via `landing-zone` OpenTofu or equivalent).
