# Integrating with eks-gitops

`eks-agent-platform` is the **product**. It builds two things:

- **The operator** — a Helm chart at `charts/operator/` (CRDs + Deployment + RBAC), published to `ghcr.io/nanohype/eks-agent-platform/operator`.
- **The terraform** — per-tenant AWS state under `terraform/components/` (agent IRSA, Bedrock access, egress, kill-switch, eval-runtime, cost pipeline, batch runtime, model artifacts, accelerator pools).

It is not a deploy catalog. Nothing in this repo applies itself to a cluster.

[`eks-gitops`](https://github.com/nanohype/eks-gitops) is the **deploy catalog**. It git-sources the operator chart from this repo, supplies per-environment values, and reconciles everything onto clusters with ArgoCD. The split is clean: this repo decides _what the operator is_, eks-gitops decides _where and how it runs_.

## What eks-gitops deploys

- **The operator** — `applicationsets/addons-agent-operator.yaml` git-sources `charts/operator`. It injects the per-cluster IRSA role ARNs and EKS OIDC wiring (provider ARN + issuer host) from the cluster-Secret annotations that `cluster-bootstrap` sets, so no account-specific values are ever committed to the chart. The same ApplicationSet injects the eval-runner role ARN and report bucket (see eval-runtime below).
- **kagent + agentgateway** — `applicationsets/addons-ai-platform.yaml`.
- **Argo Workflows / Rollouts / Events** — `applicationsets/addons-argo-platform.yaml`. These are prerequisites for the operator's eval-runtime and SLO.
- **Accelerators** — the GPU operator, NVIDIA DRA driver, and AWS Neuron device plugin land via the `accelerators` category: `applicationsets/addons-accelerators-{helm,kustomize}.yaml`, with values under `addons/accelerators/<addon>/` (gpu sync wave 6, dra wave 7, neuron wave 6).
- **Dashboards** — the seven persona dashboards ship as `GrafanaDashboard` CRs under `dashboards/base/platform/agent-*.yaml`.

eks-gitops also provides the substrate the operator assumes: ArgoCD, cert-manager, external-secrets, ALB Controller, External DNS, Cilium, Kyverno, the prometheus-operator CRDs, Loki, Tempo, Grafana, OpenCost, and the rest of the cluster addon catalog.

## What ships inside the operator chart

The operator owns its own runtime. Two subsystems ride along in `charts/operator/` (chart 0.2.0), each behind a values toggle:

- **eval-runtime** (`evalRuntime.*`, on by default) — the Argo `WorkflowTemplate` (eval-runner) the operator submits `EvalSuite` runs to, the gating `AnalysisTemplate`, and the `eval-runner` namespace + ServiceAccount + RBAC the workflow pods run under. Source: `charts/operator/{files,templates}/eval-runtime/`. Needs the Argo Workflows CRD (from `addons-argo-platform`). The eval-runner IRSA role ARN and the S3 report bucket carry the AWS account id, so eks-gitops injects them per-cluster via `addons-agent-operator.yaml`; they're empty in the base chart. The Rollouts `AnalysisTemplate` (`evalRuntime.rollouts.enabled`) is off by default since it needs the Argo Rollouts CRD.
- **operator SLO** (`slo.*`, on by default) — the operator's own observability: a `PrometheusRule` (recording rules + alerts), the kube-state-metrics `CustomResourceState` config that makes `kube_customresource_*` metrics exist, and an `AlertmanagerConfig` for persona alert routing. Source: `charts/operator/{files,templates}/slo/`. Needs the prometheus-operator CRDs. Alert routing (`slo.alerting.enabled`) is off by default because its receivers reference external Secrets that must pre-exist.

## How a cluster opts in

A cluster joins the agent platform by carrying the label `eks-agent-platform/enabled=true`. `cluster-bootstrap` (in `landing-zone`) sets it, along with the cluster-Secret annotations that supply the per-cluster IRSA/OIDC values. The eks-gitops ApplicationSets select on that label, so once the cluster is bootstrapped the operator, AI platform, Argo platform, accelerators, and dashboards flow in automatically in sync-wave order.

## Validating before you ship

Everything this repo produces is validated locally and in CI:

```bash
task helm:lint        # lint every chart in charts/
task helm:template    # render every chart against defaults
task tofu:validate    # validate every OpenTofu component
task ci               # full local gate: fmt:check, lint, validate, typecheck, test
```
