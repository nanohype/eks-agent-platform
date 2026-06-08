# Architecture — Eval gating flow

How an `EvalSuite` becomes a quality gate on a tenant's canary / blue-green deploy.

## Provisioning

```mermaid
sequenceDiagram
  participant User
  participant K8s as kube-apiserver
  participant ER as EvalReconciler
  participant Argo as Argo Workflows

  User->>K8s: kubectl apply EvalSuite<br/>{schedule: '0 6 * * *', cases: [...]}
  K8s->>ER: reconcile event
  ER->>K8s: Get Platform, Get AgentFleet
  ER->>ER: gate: both Ready?
  ER->>Argo: CreateOrUpdate CronWorkflow<br/>workflowTemplateRef=eval-runner<br/>arguments: {platform, fleet, suite-name, cases-inline, pass-threshold}
  ER->>K8s: EvalSuite.status.phase=Provisioning<br/>condition EvalReconciled=True
```

## Execution

```mermaid
sequenceDiagram
  participant Cron as Argo CronWorkflow
  participant WF as Argo Workflow
  participant RC as resolve-cases task
  participant RN as run-cases task
  participant Score as score task
  participant WB as writeback task
  participant K8s as kube-apiserver
  participant AGW as agentgateway
  participant S3

  Cron->>WF: spawn Workflow (06:00 UTC daily)
  WF->>RC: resolve-cases
  alt cases-inline non-empty
    RC->>RC: parse inline JSON
  else cases-manifest set
    RC->>S3: aws s3 cp manifest.json
  end
  RC-->>WF: /tmp/cases.json

  WF->>RN: run-cases (eval-runner image)
  loop per case
    RN->>AGW: POST /v1/agents/{platform}-{fleet}/messages
    AGW-->>RN: response + latency
    RN->>RN: record {output, latency_ms, cost_usd, error?}
  end
  RN-->>WF: /tmp/results.json

  WF->>Score: score
  Score->>Score: pass/fail per case<br/>· checks expectContains + maxLatencyMs + maxCostUsd<br/>· compute mean score
  Score->>S3: upload report.html<br/>+ junit.xml
  Score-->>WF: /tmp/score.json {meanScore, passed, reportUrl}

  WF->>WB: writeback
  WB->>K8s: kubectl patch evalsuite {suite-name}<br/>--namespace {tenant-ns}<br/>--subresource status<br/>--type merge<br/>--patch {lastScore, lastRunAt, lastReportUrl, phase}
```

## Gating a rollout

```mermaid
sequenceDiagram
  participant Dev as Tenant deploy
  participant ARO as Argo Rollouts
  participant AT as AnalysisRun
  participant K8s as kube-apiserver
  participant ES as EvalSuite.status

  Dev->>ARO: kubectl apply Rollout w/ canary + analysis.templates: [eval-suite-gate]
  ARO->>ARO: 25% canary live
  ARO->>AT: spawn AnalysisRun

  loop every 30s, up to 60 measurements (≈30 min)
    AT->>K8s: GET evalsuite/{name}
    K8s-->>AT: status.lastScore + status.phase
    AT->>AT: result.score=lastScore<br/>result.passed=(phase=='Passed' ? 1 : 0)
    alt result.passed == 1 AND result.score >= pass-threshold
      Note over AT: successCondition matches → success measurement
    else result.passed == 0
      Note over AT: failureCondition matches → failure measurement
    end
  end

  alt failureLimit (3) reached
    AT->>ARO: AnalysisRun: Failed
    ARO->>ARO: abort rollout, scale canary to 0
  else
    AT->>ARO: AnalysisRun: Successful
    ARO->>ARO: promote canary to 100%
  end
```

## Critical: EvalSuite must be Ready BEFORE the rollout

The AnalysisRun polls `status.lastScore`. If the suite has never run (Provisioning), `lastScore` is empty and the gate sits at `initialDelay`. A 30m `initialDelay` is the runway; after that, the rollout aborts.

Practice: schedule the eval daily on a CronWorkflow, then any rollout the same day uses the most recent score as the gate. If the eval failed last night, the rollout fails fast; no canary traffic exposed to a regression.

See [ADR 0008 — Eval-runtime ships inside the operator chart](../adr/0008-eval-runtime-operator-chart.md) for why the WorkflowTemplate ships in `charts/operator` behind the `evalRuntime.*` toggles. See [charts/operator/files/eval-runtime/analysis-template.yaml](../../charts/operator/files/eval-runtime/analysis-template.yaml) for the literal AnalysisTemplate spec.
