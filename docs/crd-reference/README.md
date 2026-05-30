# CRD reference

Browsable Markdown reference, regenerated from the Go types on every `make -C operators manifests`:

- [`v1alpha1.md`](./v1alpha1.md) — all eight kinds, every field, every validation marker.

The **source of truth** is the Go types in `operators/api/{platform,agents,governance}/v1alpha1/` plus the generated manifests in `operators/config/crd/bases/`; this Markdown is a rendered view of them.

Each Go type carries:

- kubebuilder validation markers (regex / enum / minimum / pattern constraints),
- kubebuilder default markers,
- per-field godoc explaining intent.

The generated YAML manifests are the operational truth — they're what gets applied to the cluster and what the apiserver validates `kubectl apply` against.

## Kinds

| Kind                                    | Group                     | Scope      | Go type                                                                                  | Generated CRD manifest                                                                                                        |
| --------------------------------------- | ------------------------- | ---------- | ---------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `Tenant`                                | `platform.nanohype.dev`   | Cluster    | [`tenant_types.go`](../../operators/api/platform/v1alpha1/tenant_types.go)               | [`platform.nanohype.dev_tenants.yaml`](../../operators/config/crd/bases/platform.nanohype.dev_tenants.yaml)                   |
| `Platform`                              | `platform.nanohype.dev`   | Namespaced | [`platform_types.go`](../../operators/api/platform/v1alpha1/platform_types.go)           | [`platform.nanohype.dev_platforms.yaml`](../../operators/config/crd/bases/platform.nanohype.dev_platforms.yaml)               |
| `ModelGateway` (+ `ModelRoute`)         | `agents.nanohype.dev`     | Namespaced | [`modelgateway_types.go`](../../operators/api/agents/v1alpha1/modelgateway_types.go)     | [`agents.nanohype.dev_modelgateways.yaml`](../../operators/config/crd/bases/agents.nanohype.dev_modelgateways.yaml)           |
| `AgentFleet` (+ `Agent`, `ScalingSpec`) | `agents.nanohype.dev`     | Namespaced | [`agentfleet_types.go`](../../operators/api/agents/v1alpha1/agentfleet_types.go)         | [`agents.nanohype.dev_agentfleets.yaml`](../../operators/config/crd/bases/agents.nanohype.dev_agentfleets.yaml)               |
| `AgentSandbox`                          | `agents.nanohype.dev`     | Namespaced | [`agentsandbox_types.go`](../../operators/api/agents/v1alpha1/agentsandbox_types.go)     | [`agents.nanohype.dev_agentsandboxes.yaml`](../../operators/config/crd/bases/agents.nanohype.dev_agentsandboxes.yaml)         |
| `SandboxPool` (+ `SandboxScalingSpec`)  | `agents.nanohype.dev`     | Namespaced | [`sandboxpool_types.go`](../../operators/api/agents/v1alpha1/sandboxpool_types.go)       | [`agents.nanohype.dev_sandboxpools.yaml`](../../operators/config/crd/bases/agents.nanohype.dev_sandboxpools.yaml)             |
| `BudgetPolicy`                          | `governance.nanohype.dev` | Namespaced | [`budgetpolicy_types.go`](../../operators/api/governance/v1alpha1/budgetpolicy_types.go) | [`governance.nanohype.dev_budgetpolicies.yaml`](../../operators/config/crd/bases/governance.nanohype.dev_budgetpolicies.yaml) |
| `EvalSuite`                             | `governance.nanohype.dev` | Namespaced | [`evalsuite_types.go`](../../operators/api/governance/v1alpha1/evalsuite_types.go)       | [`governance.nanohype.dev_evalsuites.yaml`](../../operators/config/crd/bases/governance.nanohype.dev_evalsuites.yaml)         |

Regenerate manifests + DeepCopy: `make -C operators manifests` (also copies to `charts/operator/crds/`).

## Field-level docs

Read the Go types directly — godoc comments are the canonical field description. `go doc` against any of the type files renders the field-by-field reference at terminal speed.

## Regenerating

[`v1alpha1.md`](./v1alpha1.md) is rendered by [elastic/crd-ref-docs](https://github.com/elastic/crd-ref-docs), wired into the `manifests` target (config: [`operators/hack/crd-ref-docs.yaml`](../../operators/hack/crd-ref-docs.yaml)). Whenever you edit kubebuilder annotations or godoc on a type, `make -C operators manifests` regenerates CRDs, copies them into the Helm chart, and re-renders this Markdown — no manual sync needed.
