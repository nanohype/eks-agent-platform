# Contributing

## Workflow

1. Branch from `main` with a conventional prefix: `feat/`, `fix/`, `chore/`, `docs/`, `refactor/`, `test/`.
2. Run `task ci` locally before pushing. CI must pass.
3. Conventional commits enforced via commitlint. Use the structured commit-message format from `~/.claude/CLAUDE.md` (section headers, file-level detail, scaled verbosity).
4. Open a PR. Reviews are required for changes under `operators/api/`, `charts/`, `terraform/components/`, `gitops/applicationsets/`.

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

See [README.md](./README.md#what-you-get) and [ARCHITECTURE.md](./ARCHITECTURE.md).

## Adding a CRD

1. Scaffold in `operators/api/v1alpha1/` via `kubebuilder create api`.
2. Add a reconciler in `operators/internal/controller/`.
3. Add validating/defaulting webhooks if needed in `operators/internal/webhook/`.
4. Regenerate CRD manifests with `task operator:gen` — outputs to `operators/config/crd/bases/` and `charts/operator/crds/`.
5. Regenerate the TS client in `packages/client/src/generated/` via `task client:gen`.
6. Document the kind in `docs/crd-reference/<kind>.md`.
7. Add a conformance test in `operators/test/conformance/`.

## Adding an OpenTofu component

1. Create `terraform/components/<name>/` with `main.tf`, `variables.tf`, `outputs.tf`, `versions.tf`, `README.md`.
2. Add a Terragrunt unit in `terraform/live/<env>/<name>/terragrunt.hcl`.
3. Outputs published to SSM under `/eks-agent-platform/<env>/<component>/<key>`.
4. Add `task tofu:validate` coverage.

## Adding a GitOps addon

1. Add to `gitops/applicationsets/<category>.yaml` (matrix generator with `clusters` + `list`).
2. Add base + per-env values to `gitops/addons/<name>/`.
3. Document in `docs/personas/` if it changes user-visible behavior.

## Adding a TS package

1. `mkdir packages/<name>` with `package.json` (scope `@eks-agent/`), `tsconfig.json`, `src/index.ts`.
2. Add to `pnpm-workspace.yaml` packages list if it's outside the existing globs.
3. Resolve all dep versions via `pnpm view`, never hand-pin.

## Releases

- Each component publishes independently with conventional-commit-driven version bumps via Changesets.
- Operator images signed with cosign + SBOM via syft on every tagged release.
- Helm charts published to OCI registry under `oci://ghcr.io/stxkxs/eks-agent-platform/charts/`.

## Code of Conduct

See [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md).
