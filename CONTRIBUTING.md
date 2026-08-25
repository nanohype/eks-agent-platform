# Contributing

## Workflow

1. Branch from `main` with a conventional prefix: `feat/`, `fix/`, `chore/`, `docs/`, `refactor/`, `test/`.
2. Run `task ci` locally before pushing. CI must pass.
3. Conventional commits enforced via commitlint. Write the body as structured documentation: section headers for large changes, file-level detail where it matters, verbosity scaled to scope.
4. Open a PR. Reviews are required for changes under `operators/api/`, `charts/`, `terraform/components/`.

## Local prereqs

| Tool          | Version                                           |
| ------------- | ------------------------------------------------- |
| `tofu`        | resolved at scaffold time, matches `landing-zone` |
| `terragrunt`  | latest                                            |
| `kubectl`     | matches target EKS minor version                  |
| `helm`        | resolved at scaffold time                         |
| `argocd` CLI  | resolved at scaffold time                         |
| `pnpm`        | see root `package.json` engines                   |
| `node`        | see root `package.json` engines                   |
| `go`          | see `operators/go.mod`                            |
| `kubebuilder` | v4                                                |
| `task`        | latest                                            |
| `kind`        | for local conformance tests                       |

## Layout

See [README.md](./README.md#layout) and [ARCHITECTURE.md](./ARCHITECTURE.md).

## Adding a CRD

1. Scaffold in `operators/api/<group>/v1alpha1/` (`platform`, `agents`, or `governance`) via `kubebuilder create api`.
2. Add a reconciler in `operators/internal/controller/`.
3. Add cross-field validation as CEL `+kubebuilder:validation:XValidation` markers on the API types (CRD schema is the floor; CEL enforces the invariants at admission).
4. Regenerate CRD manifests with `task operator:gen` — outputs to `operators/config/crd/bases/` and `charts/operator/crds/`.
5. Mirror any new spec/status field into the zod schemas in `packages/core/src/schemas.ts`. The TypeScript side is hand-written rather than generated, and `pnpm check:schema-drift` diffs it against the generated CRD OpenAPI so a field present on one side and not the other fails the build.
6. Confirm `docs/crd-reference/v1alpha1.md` picked up the new kind — `make -C operators manifests` re-renders it from godoc. There are no per-kind files.
7. Add a conformance test in `operators/test/conformance/`.

## Adding an OpenTofu component

1. Create `terraform/components/<name>/` with `main.tf`, `variables.tf`, `outputs.tf`, `versions.tf`, `README.md`.
2. Add a Terragrunt unit in `terraform/live/<env>/<name>/terragrunt.hcl`.
3. Outputs published to SSM under `/eks-agent-platform/<cluster>/<component>/<key>`.
4. Add `task tofu:validate` coverage.

## Cluster addons

Agent cluster addons — Envoy AI Gateway, the Argo platform, and the persona dashboards — live in `eks-gitops`, not here. See its docs for adding or tuning one. This repo's job is to build the artifacts they deploy: the operator chart (`charts/operator`) and the terraform components.

The operator's own eval-runtime rides inside `charts/operator` behind the `evalRuntime.*` values toggle — edit the chart, not a separate addon. The operator's alert rules are Grafana-managed in eks-gitops.

## Adding a TS package

1. `mkdir packages/<name>` with `package.json` (scope `@eks-agent/`), `tsconfig.json`, `src/index.ts`.
2. Add to `pnpm-workspace.yaml` packages list if it's outside the existing globs.
3. Resolve all dep versions via `pnpm view`, never hand-pin.

## Releases

- Each component publishes independently with conventional-commit-driven version bumps via Changesets.
- Operator images signed with cosign + SBOM via syft on every tagged release.
- Helm charts published to OCI registry under `oci://ghcr.io/nanohype/eks-agent-platform/charts/`.

## Code of Conduct

See [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md).
