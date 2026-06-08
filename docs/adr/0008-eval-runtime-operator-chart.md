# ADR 0008 — eval-runtime + operator SLO ship in the operator chart, not a gitops overlay

## Status

Accepted (2026-06-07). Supersedes [ADR 0007](0007-eval-runtime-kustomize.md).

## Context

The cluster-delivery of the agent platform consolidated into one catalog. Cluster addons
(operator, agentgateway, kagent, GPU/Neuron, argo) are deployed by `eks-gitops`; this repo
_builds_ the operator (chart, CRDs, terraform) and is no longer a deploy catalog. The
`gitops/` overlay — a second ArgoCD source that duplicated operator/agentgateway/kagent and
also carried the eval-runtime + SLO kustomize — was retired.

That left two homes for the operator's own runtime pieces — the eval-runtime
(`WorkflowTemplate` + `AnalysisTemplate` + Namespace/SA/RBAC) and the operator SLO
(`PrometheusRule` + `AlertmanagerConfig` + the kube-state-metrics CR-state config):

1. **The eks-gitops catalog** as standalone addons (where the generic addons now live).
2. **The operator Helm chart** (`charts/operator/`), shipped with the operator it observes.

## Decision

Option 2. The eval-runtime and SLO manifests fold into `charts/operator/` behind
`evalRuntime.*` and `slo.*` values toggles (chart 0.2.0). eks-gitops deploys the chart and
enables them per-env. The CR-heavy, mustache-bearing manifests (`WorkflowTemplate`,
`AnalysisTemplate`, `PrometheusRule`, `AlertmanagerConfig`) ship under `charts/operator/files/`
and are emitted verbatim via `.Files.Get` so Helm never evaluates the Argo/Prometheus/Alertmanager
`{{...}}`; only env-specific literals (bucket, gateway URL, namespaces) are substituted.

## Why the ADR-0007 blockers no longer apply

ADR 0007 chose kustomize-in-gitops over the operator chart, on five grounds. The consolidation
changes the calculus:

- **Source-vs-deploy is the real split (new framing).** These manifests are _the operator's own
  runtime_ — its eval pipeline and its SLOs. Their _source_ belongs with the product (this repo);
  their _deploy_ belongs with the catalog (eks-gitops). Folding them into the chart satisfies both:
  source lives here, eks-gitops deploys the chart. A standalone eks-gitops addon would put the
  _source_ of operator-specific manifests in the deploy repo — the inversion the consolidation removes.
- **Blocker #3 (Argo CRD chicken-and-egg) is resolved by toggles + sync-wave ordering.** The
  `AnalysisTemplate` (needs the Argo Rollouts CRD) is gated behind `evalRuntime.rollouts.enabled`,
  default **off**. The `WorkflowTemplate` renders by default but eks-gitops orders Argo
  (`addons-argo-platform`, waves 50-52) and the prometheus-operator CRDs (bootstrap wave 0) ahead
  of the operator (wave 21), with the Application's retry backoff as the backstop. Helm install no
  longer fails on a missing CRD because the toggle + ordering, not chart packaging, gates rendering.
- **Lifecycle independence (reason 1) + separate audit chain (reason 4) are preserved.** The
  manifests live under `charts/operator/files/` as standalone YAML — editing the eval pipeline is
  still a `files/` change, reviewed independently, not an operator-code change. The chart version
  bumps; the operator binary (`appVersion`) does not.
- **Per-env tuning (reason 5)** moves to eks-gitops `values-<env>.yaml` (the same place the
  operator's own per-env sizing lives), via the `evalRuntime.*` / `slo.*` toggles.

The operator is still the consumer, not the publisher (reason 2): `EvalReconciler` references the
template by name (`eval-runner`), which the chart keeps byte-identical, along with the
`eval-runner` namespace + ServiceAccount that the `terraform/components/eval-runtime` IRSA trust pins.

## Trade-offs

- **External prerequisites stay external.** `slo.alerting.enabled` (the AlertmanagerConfig) needs
  six pre-existing Secrets; the CR-state ConfigMap is inert unless kube-state-metrics is started
  with `--custom-resource-state-config-file` (owned by the eks-gitops KSM addon). Both default to a
  safe state (alerting off; the ConfigMap ships but is harmless unmounted) and are documented in the
  chart NOTES + README.
- **The eval IRSA role ARN is injected, not in-chart.** It embeds the AWS account id, so the
  eks-gitops `addons-agent-operator` ApplicationSet injects it from a cluster-Secret annotation —
  the same pattern as the operator role.

## Cross-references

- Implementation: `charts/operator/{files,templates}/{eval-runtime,slo}/`, `charts/operator/values.yaml`.
- Consumer: `operators/internal/controller/eval_reconcile.go`.
- Terraform-side: `terraform/components/eval-runtime/` (IRSA + SSM publication).
- Deploy + per-env enablement: `eks-gitops/applicationsets/addons-agent-operator.yaml`,
  `eks-gitops/addons/ai-platform/operator/values*.yaml`.
- Flow diagram: [`docs/architecture/eval-gating-flow.md`](../architecture/eval-gating-flow.md).
