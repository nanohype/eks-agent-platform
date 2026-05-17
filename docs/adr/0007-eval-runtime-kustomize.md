# ADR 0007 — eval-runtime WorkflowTemplate ships via kustomize, not the operator chart

## Status

Accepted (2026-05-16).

## Context

The eval-runtime deliverable includes:

- the per-eval-runner IRSA role (AWS, terraform),
- a Kubernetes Namespace + ServiceAccount + ClusterRole + ClusterRoleBinding (k8s, gitops),
- an Argo Workflows `WorkflowTemplate` (k8s, ~280 lines of pipeline definition),
- an Argo Rollouts `AnalysisTemplate` (k8s).

Two options for where the k8s-side pieces live:

1. **Inside the operator chart** (`charts/operator/templates/`) — packaged with the operator binary, version-locked to the operator release.
2. **In gitops/addons/eval-runtime/ as a kustomize package** — separate ArgoCD Application, delivered alongside other addons (agentgateway, kagent, KEDA, etc.).

## Decision

Option 2. The `WorkflowTemplate` + `AnalysisTemplate` + Namespace + SA + RBAC ship via the `kustomize-only` ArgoCD ApplicationSet entry `eval-runtime` at syncWave 30.

## Why

1. **Lifecycle independence.** Updating the eval-runner pipeline (e.g., adding a new scoring metric) shouldn't require an operator release. The pipeline is tenant-facing surface; it evolves on a faster cadence than the operator's reconcile loop. Putting it in the operator chart couples those cadences artificially.

2. **The operator is the consumer, not the publisher.** The operator's `EvalReconciler.ensureArgoWorkflow` references the template by name (`workflowTemplateRef.name: eval-runner`) — it doesn't care where the template was provisioned from. The `runner_namespace` + `runner_service_account` are resolved from SSM at operator startup, not hardcoded.

3. **Argo Workflows is a hard dependency.** The `WorkflowTemplate` CR can't be applied until Argo Workflows CRDs are installed. If the operator chart shipped it, helm install would fail until Argo Workflows was already installed — chicken-and-egg. The kustomize ApplicationSet's `syncWave: 30` ensures Argo Workflows (`syncWave: 25`) is already in place.

4. **Separate audit chain.** Changes to the eval pipeline land in `gitops/addons/eval-runtime/` PRs with their own reviewers (eval/quality team) rather than in operator-internal PRs (platform/SRE). The operator's PR surface stays focused on the reconciler implementation.

5. **Per-environment tuning.** Different environments may want different pipelines (e.g., a faster scoring template in dev, stricter pass-thresholds in prod). Kustomize overlays per env are the standard pattern; embedding the template in the operator chart would require per-env Helm values + template conditionals — messier.

## Trade-offs

- **Two-repo coordination.** Operator + eval-runtime ship in the same repo so this isn't actually two repos, but it is two ArgoCD Applications. A breaking change to the operator's parameter contract (e.g., renaming `suite-name` to `suite_name`) requires coordinated changes to both. Documented in the eval reconciler's source comments + the `WorkflowTemplate`'s top-of-file contract comment.

- **No type-checking across the boundary.** The operator emits parameters; the WorkflowTemplate consumes them. If the operator emits a parameter the template doesn't know about, Argo silently ignores it. A quality-check caught one example (the original `suite` param's shell-strip bug); we mitigate via the contract comment + the operator's eval reconciler tests covering the emit path.

- **SA annotation needs per-env IRSA role ARN.** The `serviceaccount.yaml` ships with `REPLACE_BY_APPLICATIONSET` as the annotation value; the ApplicationSet patch (or post-apply kubectl annotate) fills it in. Workable but a footgun for first-time operators who forget the patch step. Could be cleaned up with a future ApplicationSet kustomize-patch entry.

## Alternatives considered

- **Operator chart.** Rejected for reasons 1-5.
- **Standalone Helm chart in `charts/eval-runtime/`.** Half-step: more structure than kustomize, less than a full chart. Doesn't meaningfully add value over kustomize for ~5 simple manifests with no templating needs.
- **Operator-managed: operator creates the WorkflowTemplate itself at startup.** Tempting (one source of truth, no gitops drift) but couples operator-restart latency to template availability and removes the gitops change-management benefit (every template change is now an operator code change requiring a release).

## Cross-references

- Implementation: `gitops/addons/eval-runtime/` (kustomize), `operators/internal/controller/eval_reconcile.go` (the consumer).
- Terraform-side: `terraform/components/eval-runtime/` (IRSA + SSM publication of `runner_namespace` / `runner_service_account` / `runner_role_arn`).
- ApplicationSet wiring: `gitops/applicationsets/kustomize-only.yaml`.
- Flow diagram: [`docs/architecture/eval-gating-flow.md`](../architecture/eval-gating-flow.md).
