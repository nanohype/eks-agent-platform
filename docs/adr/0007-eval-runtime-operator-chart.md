# ADR 0007 — the eval-runtime ships in the operator chart, not a gitops overlay

## Status

Accepted (2026-06-07).

## Context

The cluster-delivery of the agent platform consolidated into one catalog. Cluster addons
(operator, envoy-ai-gateway, GPU/Neuron, argo) are deployed by `eks-gitops`; this repo
_builds_ the operator (chart, CRDs, terraform) and is no longer a deploy catalog. The
`gitops/` overlay — a second ArgoCD source that duplicated operator/gateway — is not
part of the delivery path.

That leaves two candidate homes for the operator's own runtime — the eval-runtime
(`WorkflowTemplate` + `AnalysisTemplate` + Namespace/SA/RBAC):

> **Scope.** This decision governs the eval-runtime. The operator ships no SLO
> manifests: the catalog installs the prometheus-operator CRDs and no ruler, so
> a chart-shipped `PrometheusRule` or `AlertmanagerConfig` would be applied
> everywhere and evaluated nowhere, and the kube-state-metrics CR-state config
> belongs to the eks-gitops KSM addon.

1. **The eks-gitops catalog** as standalone addons (where the generic addons now live).
2. **The operator Helm chart** (`charts/operator/`), shipped with the operator it observes.

## Decision

Option 2. The eval-runtime manifests fold into `charts/operator/` behind the
`evalRuntime.*` values toggle. eks-gitops deploys the chart and enables it per-env. The
CR-heavy, mustache-bearing manifests (`WorkflowTemplate`, `AnalysisTemplate`) ship under
`charts/operator/files/` and are emitted verbatim via `.Files.Get` so Helm never evaluates
the Argo `{{...}}`; only env-specific literals (bucket, gateway URL, namespaces) are
substituted.

## Why the ADR-0007 blockers no longer apply

ADR 0007 chose kustomize-in-gitops over the operator chart, on five grounds. The consolidation
changes the calculus:

- **Source-vs-deploy is the real split (new framing).** These manifests are _the operator's own
  runtime_ — its eval pipeline. Its _source_ belongs with the product (this repo);
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
  operator's own per-env sizing lives), via the `evalRuntime.*` toggle.

The operator is still the consumer, not the publisher (reason 2): `EvalReconciler` references the
template by name (`eval-runner`), which the chart keeps byte-identical, along with the
`eval-runner` namespace + ServiceAccount that the `terraform/components/eval-runtime` IRSA trust pins.

## Trade-offs

- **External prerequisites stay external.** The `AnalysisTemplate` needs the Argo Rollouts
  CRD and is off by default. The `kube_customresource_*` metrics the platform's alerts read are
  defined by the eks-gitops kube-state-metrics addon's customResourceState config — the single
  source KSM actually loads. This chart consumes those metrics; it does not ship its own
  (duplicate) copy.
- **The eval IRSA role ARN is injected, not in-chart.** It embeds the AWS account id, so the
  eks-gitops `addons-agent-operator` ApplicationSet injects it from a cluster-Secret annotation —
  the same pattern as the operator role.

## Cross-references

- Implementation: `charts/operator/{files,templates}/eval-runtime/`, `charts/operator/values.yaml`.
- Consumer: `operators/internal/controller/eval_reconcile.go`.
- Terraform-side: `terraform/components/eval-runtime/` (IRSA + SSM publication).
- Deploy + per-env enablement: `eks-gitops/applicationsets/addons-agent-operator.yaml`,
  `eks-gitops/addons/ai-platform/operator/values*.yaml`.
- Flow diagram: [`docs/architecture/eval-gating-flow.md`](../architecture/eval-gating-flow.md).
