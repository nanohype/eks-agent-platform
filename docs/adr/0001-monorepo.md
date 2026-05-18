# ADR 0001 — Monorepo over multi-repo

## Status

Accepted (2026-05-15).

## Context

Initial design proposed splitting `eks-agent-platform` into five repos: operators (Go), gitops (YAML), SDK (TS), terraform overlay, examples.

## Decision

One monorepo with polyglot workspaces:

- `operators/` — Go (kubebuilder v4)
- `charts/` — Helm
- `gitops/` — ArgoCD ApplicationSets
- `packages/` — TypeScript (pnpm + turbo)
- `terraform/` — OpenTofu + Terragrunt
- `examples/`, `docs/`

## Why

1. **Rate of change is correlated.** Adding a field to a CRD touches the Go type, the controller, the Helm template, the TS client, the docs, and probably an example — all together. A multi-repo split means six PRs; a monorepo means one.
2. **Convention.** Sibling repos (`landing-zone`, `eks-gitops`) are polyglot monorepos. This repo follows that pattern.
3. **Release independence preserved.** Per-component tags (`operator-v0.3.1`, `charts-v0.3.1`, `sdk-v0.3.1`) give Renovate-friendly version bumps without needing separate repos.

## Consequences

- CI matrix is larger but cacheable (turbo + go build cache + tofu validate).
- The Go module under `operators/` is the canonical Go path for the CRD types; downstream Go consumers import `github.com/nanohype/eks-agent-platform/operators/api/v1alpha1`.
- Doc cross-references resolve to relative paths, which keeps them stable across forks.
