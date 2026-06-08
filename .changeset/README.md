# Changesets

This repo uses [changesets](https://github.com/changesets/changesets) to manage versioning + the CHANGELOG for the `@eks-agent/*` npm packages.

## When to add a changeset

Add a changeset for any PR that touches a published package (`packages/core`, `packages/sdk`, `packages/pricing`, `packages/client`, `packages/cli`). The `[email protected]` GitHub Action on the release workflow will refuse to publish if a changed published package has no changeset entry covering it.

Skip the changeset for PRs that only touch:

- `operators/` (Go module — released via image tags, not npm)
- `charts/` (Helm — released via OCI tags)
- `terraform/`, `docs/`, `examples/`
- Pure infra changes (workflows, hooks, configs)

## How

```bash
pnpm changeset
```

Walk the prompts. Pick package(s) affected; pick version bump (`major` for breaking changes, `minor` for new features, `patch` for fixes); write the changelog entry as if it were a release note. The CLI writes a markdown file in this directory; commit it as part of your PR.

## Linked packages

The five `@eks-agent/*` packages are **linked** in `config.json` — any minor bump on one of them bumps all five together. They share a build matrix and an internal dep graph; releasing `sdk@0.3.0` without bumping `core` to the same minor would risk type-mismatch confusion for consumers. The CLI scope is part of the link group because `agentctl` ships against pinned `@eks-agent/core` types.

## Commit-type convention

Commit types like `feat:` and `fix:` do **not** auto-generate changesets — that's deliberate. Conventional-commits drives the commit-message gate (commitlint), changesets drives the release-bump gate. Both are required because they answer different questions: commitlint says "is this commit's intent legible?"; changesets says "what version should we ship?".

`harden:` commits typically touch infra/security and don't need a changeset, but if a `harden:` commit happens to touch a published package (e.g., adding a security-related option to `@eks-agent/sdk`), include a changeset.
