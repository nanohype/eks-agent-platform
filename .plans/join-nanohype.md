# Join nanohype org

Tactical plan for moving `stxkxs/eks-agent-platform` → `nanohype/eks-agent-platform`.

Master plan: `/Users/bs/.claude/plans/so-i-want-to-snazzy-sun.md` Phase 1.4.

## Transfer

```sh
gh repo transfer stxkxs/eks-agent-platform nanohype
git remote set-url origin git@github.com:nanohype/eks-agent-platform.git
```

## Cross-references to fix

This repo has the heaviest cross-reference surface (Go operator, TypeScript SDK, Helm charts, OpenTofu components, GitOps manifests). Audit thoroughly:

```sh
grep -rn "stxkxs" --include="*.md" --include="*.yaml" --include="*.ts" --include="*.go" --include="*.hcl" --include="*.tf"
```

Specific locations to verify:

- `ARCHITECTURE.md` — references to `eks-gitops`, `aws-eks` (master plan open question)
- `README.md` — companion repo links
- `package.json` (root + packages) — repository URL fields
- `go.mod` — module path is `github.com/nanohype/eks-agent-platform/operators`. Stable identifier; leave it alone (changing a Go module path is a breaking change for any importers).
- CRD groups — the eight CRDs sit under three capability groups on the `nanohype.dev` domain: `platform.nanohype.dev` (Tenant, Platform), `agents.nanohype.dev` (AgentFleet, ModelGateway, AgentSandbox, SandboxPool), `governance.nanohype.dev` (BudgetPolicy, EvalSuite). The kubebuilder `domain` is `nanohype.dev` with `multigroup: true`. Finalizers, label/tag keys, and the leader-election lease ID follow the same domain.
- Helm chart `Chart.yaml` files — `home` / `sources` fields
- Tofu component `versions.tf` files — source URLs
- OCI image references in `gitops/applicationsets/` — registry paths

## Decisions to make during execution

The Go module path is independent of GitHub ownership, but the CRD API groups are org-aligned:

- **Keep** `github.com/nanohype/eks-agent-platform/operators` as the Go module path (stable identifier)
- **CRD API groups** are org-aligned on the `nanohype.dev` domain (`{platform,agents,governance}.nanohype.dev`)
- **Change** README/ARCHITECTURE.md companion links to point at `nanohype/*`
- **Change** GitHub-specific paths (Actions workflows, container registry refs if `ghcr.io/stxkxs/*`)

## OIDC and registries

- If GitHub Actions push to `ghcr.io/stxkxs/eks-agent-platform-*`, those container references propagate to gitops/. Decide whether to dual-publish for a window or hard-cut
- AWS OIDC trust policies for any deploy/test workflows — same pattern as landing-zone

## Verification

```sh
gh repo view nanohype/eks-agent-platform                               # 200
make manifests                                                         # CRDs still generate
make build                                                             # operator binary builds
make test                                                              # tests pass
grep -rn "github.com/stxkxs/eks-agent-platform" --include="*.go" --include="*.md"   # zero or intentional
```

## Notes

- The operator's IRSA role and IAM policies are deployment-time concerns, not in-repo refs
- Helm charts in `charts/` may have their own internal references — sweep separately
- OTel attribute names (`agents.tenant`, `agents.platform`, etc.) are not GitHub-coupled
