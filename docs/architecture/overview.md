# Architecture — Overview

## CR hierarchy

`Tenant` is cluster-scoped; everything else lives in the management namespace (`eks-agent-platform` by convention). The operator provisions a per-Platform tenant workload namespace (`tenants-<platform>`) where agent pods + the SA actually run.

```mermaid
flowchart TD
  subgraph cluster["Kubernetes cluster"]
    Tenant["<b>Tenant</b><br/>(cluster-scoped)<br/>name=acme-mesh"]

    subgraph mgmt["namespace: eks-agent-platform"]
      Platform["<b>Platform</b><br/>name=acme-mesh-support"]
      Budget["<b>BudgetPolicy</b><br/>monthlyUsd=1500"]
      Gateway["<b>ModelGateway</b><br/>routes: triage, escalation"]
      Fleet["<b>AgentFleet</b><br/>scaling: SQS depth"]
      Eval["<b>EvalSuite</b><br/>schedule: 0 6 * * *"]
    end

    subgraph tenantns["namespace: tenants-acme-mesh-support"]
      SA["ServiceAccount<br/>tenant-runtime<br/>(IRSA annotated)"]
      NP["NetworkPolicy<br/>egress: agtgw + DNS + OTel"]
      AgentCR["kagent.dev Agent<br/>+ ModelConfig"]
      Pods["Agent pods<br/>(KEDA-scaled)"]
    end

    subgraph agtgw["namespace: agentgateway"]
      Route["agentgateway.dev Route<br/>backend: bedrock"]
    end

    subgraph evalrun["namespace: eval-runner"]
      WT["WorkflowTemplate<br/>eval-runner"]
      AT["AnalysisTemplate<br/>eval-suite-gate"]
    end
  end

  Tenant -. "spec.tenant=acme-mesh" .-> Platform
  Platform -- "spec.budget" --> Budget
  Platform -- "spec.identity" --> SA
  Platform -- "tenant ns + NetworkPolicy" --> NP
  Gateway -- "spec.platformRef" --> Platform
  Gateway -- "emits per route" --> Route
  Fleet -- "spec.platformRef" --> Platform
  Fleet -- "emits per agent" --> AgentCR
  AgentCR --> Pods
  Eval -- "spec.platformRef + agentFleetRef" --> Fleet
  Eval -- "emits CronWorkflow ref" --> WT
```

## AWS-side resources per Platform

```mermaid
flowchart LR
  Platform["<b>Platform CR</b>"] --> PR["PlatformReconciler"]

  PR --> Ns["tenant ns + quotas<br/>+ LimitRange + NetworkPolicy<br/>+ AppProject"]
  PR --> IAM["IAM role<br/>&lt;env&gt;-&lt;platform&gt;-tenant<br/>+ baseline policy"]
  PR --> KMS["KMS grant on cmk-data<br/>EncryptionContext: PlatformId"]
  PR --> S3["S3 bucket policy<br/>statements for tenant prefix"]

  IAM -. "trust" .- OIDC["EKS OIDC provider"]
  KMS -. "scoped to" .- DataKey["cmk-data<br/>(landing-zone)"]
  S3 -. "writes" .- Artifacts["artifacts bucket<br/>(model-artifacts)"]
```

## End-to-end invocation path

```mermaid
sequenceDiagram
  participant Pod as Tenant agent pod
  participant SA as IRSA token
  participant AGW as agentgateway
  participant BR as AWS Bedrock
  participant CW as CloudWatch Logs
  participant Lambda as invocation-cost-publisher
  participant CWM as agents/Bedrock metric

  Pod->>SA: assume role
  Pod->>AGW: POST /v1/messages route=primary
  AGW->>BR: InvokeModel (per-route Guardrail attached)
  BR->>CW: invocation log (modelId, tokens, identity)
  CW->>Lambda: subscription filter
  Lambda->>CWM: PutMetricData EstimatedInvocationCostUsd<br/>{PlatformId}
  BR-->>AGW: response
  AGW-->>Pod: response
```

## Reconciler responsibilities

| Reconciler | Watches                                               | Emits / mutates                                                                                                 | Re-queue cadence                                                 |
| ---------- | ----------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `tenant`   | Tenant + Platform + BudgetPolicy events (via Watches) | Tenant.status aggregate                                                                                         | 5m fallback                                                      |
| `platform` | Platform                                              | tenant ns, quotas, NetworkPolicy, AppProject, IAM role, KMS grant, S3 bucket policy statements                  | 60s when IAM wired (drift detection for kill-switch tag)         |
| `gateway`  | ModelGateway                                          | agentgateway Route per ModelRoute                                                                               | 30s when Pending (waiting on agentgateway CRDs / Platform Ready) |
| `runtime`  | AgentFleet                                            | tenant SA, fleet NetworkPolicy, kagent Agent + ModelConfig per agent, KEDA ScaledObject + TriggerAuthentication | 30s when Pending                                                 |
| `budget`   | BudgetPolicy                                          | status.{currentSpend, percentOfBudget, lastReconciled}, EventBridge breach event at 120%                        | configurable (1h prod, 5m dev)                                   |
| `eval`     | EvalSuite                                             | Argo Workflow / CronWorkflow with workflowTemplateRef=eval-runner                                               | 30s when Pending                                                 |

## Where state lives

| State                            | Source of truth                                                                                                                |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| Per-tenant access control        | IAM role tags (operator reads `agents.stxkxs.io/suspended`)                                                                    |
| Per-tenant data encryption scope | KMS grant `EncryptionContext: {PlatformId}`                                                                                    |
| Per-tenant S3 isolation          | bucket policy statements with `s3:prefix` condition                                                                            |
| Per-tenant Bedrock spend         | CUR Athena table (`resource_tags_user_platformid`) + in-flight CloudWatch metric (`agents/Bedrock:EstimatedInvocationCostUsd`) |
| Per-fleet scaling target         | KEDA ScaledObject (`aws-sqs-queue` when QueueURL set, else CPU)                                                                |
| Per-suite eval result            | EvalSuite.status (written by eval-runner Workflow via kubectl patch)                                                           |
