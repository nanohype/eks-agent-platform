# Runbook — deploy the agent platform end-to-end

How to stand up the agent platform from scratch, both locally (kx) and on real
EKS, and tear it down. This is the verified sequence — every step here was run
end-to-end on 2026-05-31, and the gaps that used to bite are fixed-forward
(noted inline). Do the steps in order; each depends on the previous.

Repos involved: `landing-zone` (AWS substrate), `eks-gitops` (ArgoCD addon
catalog), `eks-agent-platform` (the operator + agent-plane mirror), `kx` (local
kind mirror), `portal` (tenant ops UI).

---

## A. Local (kx) — control plane only, no AWS

```bash
cd kx
task up                          # kind + core stack (cilium, cert-manager, argocd, …)
task stack:ai-platform:enable    # kagent + agentgateway + the operator
```

`ai-platform:enable` now installs the operator too (built from the sibling
`eks-agent-platform` checkout + kind-loaded; override the path with
`KX_EKS_AGENT_PLATFORM_DIR`). It runs `--disable-aws`, so the AWS reconcile
(IRSA/KMS) is skipped — the k8s tenant boundary still reconciles fully.

```bash
kubectl apply -f ../eks-agent-platform/examples/blank-tenant/platform.yaml
kubectl get platform blank -n eks-agent-platform -o jsonpath='{.status.phase}'   # → Ready
/path/to/cloudgov platform audit --kubeconfig ~/.kube/config                     # k8s-side PASS
```

The one expected finding locally is `IRSA_ANNOTATION_MISSING` (no real role under
`--disable-aws`). The agent plane (ModelGateway/AgentFleet) reaches Ready against
the local kagent/agentgateway.

---

## B. Real EKS

### B0. Prereqs

- AWS creds for the target account (this org used SSO profile `stxkxs`). Confirm:
  `aws sts get-caller-identity --profile <profile>`.
- Confirm Bedrock model access in the region:
  `aws bedrock list-foundation-models --region <r> --by-provider anthropic`.
- Set the dev account ID **locally** (do NOT commit — the public repo keeps the
  `111111111111` placeholder): edit
  `landing-zone/live/aws/<account>/account.hcl` → real account ID.
- Bootstrap the Terraform state bucket once:
  `landing-zone/scripts/init-backend-aws.sh <account_id> <region>`.

Set `AWS_PROFILE=<profile>` for every `terragrunt`/`aws` command below.

### B1. Substrate (landing-zone), in dependency order

From `landing-zone/live/aws/<account>/<region>/<env>/`, `terragrunt apply` each:

1. `network` — VPC, subnets, 1 NAT, interface endpoints.
2. `cluster` — EKS + a Graviton node group. `enable_cluster_creator_admin_permissions`
   is on, so the applying principal gets cluster-admin automatically (no manual
   access entry). Then: `aws eks update-kubeconfig --name <cluster> --region <r>
--profile <profile> --alias <cluster>` → `kubectl --context <cluster> get nodes`.
3. `cluster-bootstrap` — installs cilium (ENI mode, replaces VPC-CNI) + ArgoCD +
   an app-of-apps pointing at `eks-gitops/applicationsets`. ArgoCD then syncs the
   addon catalog, including `addons-ai-platform` (kagent + agentgateway).
4. `agent-iam` — the operator IRSA role (path-scoped, boundary-gated), the tenant
   permissions boundary + baseline policies, and the SSM params the operator
   reads (`/eks-agent-platform/<env>/agent-iam/*`).
5. Per tenant, `<app>-platform` (e.g. `competitive-intelligence-platform`) for
   Aurora / per-tenant IRSA / Secrets. OIDC is wired from the `cluster`
   dependency automatically.

Confirm addons converge: `kubectl --context <cluster> get applications -n argocd`
(cert-manager, external-secrets, cilium, kagent, agentgateway, … Synced/Healthy).

### B2. Operator on EKS

Get the image. Either:

- **Published (preferred):** push a release tag in `eks-agent-platform` →
  `release.yaml` builds a multi-arch image to
  `ghcr.io/nanohype/eks-agent-platform/operator:<version>`.
- **Dev/in-account ECR:** build for the node arch and push to ECR — see the
  arch gotcha below.

Install with the AWS reconcile ON (no `--disable-aws`):

```bash
helm upgrade --install eks-agent-platform eks-agent-platform/charts/operator \
  --kube-context <cluster> -n eks-agent-platform --create-namespace \
  --set image.repository=<registry>/eks-agent-platform/operator \
  --set image.tag=<version> \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=<agent-iam operator role ARN> \
  --set config.environment=<env> --set config.region=<r> \
  --set config.ssmPathPrefix=/eks-agent-platform \
  --set config.oidc.providerArn=<cluster oidc_provider_arn> \
  --set config.oidc.issuerHost=<cluster oidc_issuer> \
  --set webhooks.certManager.installSelfSignedIssuer=true \
  --set networkPolicy.engine=cilium --wait
```

The operator loads the rest of its config (tenant IAM path, baseline +
boundary ARNs) from SSM via its IRSA role. Verify:
`kubectl --context <cluster> logs -n eks-agent-platform -l app.kubernetes.io/name=operator`
→ "AWS substrate loaded".

### B3. Deploy tenants through the portal

```bash
cd portal && docker compose up -d && task dev
```

Config the worker (env): `GITOPS_TENANTS_REPO_URL` (a tenants gitops repo),
`GITOPS_TENANTS_REPO_REF`, `GITOPS_SSH_KEY_PATH`, `GITOPS_AUTHOR_NAME/EMAIL`,
`EKS_AGENT_PLATFORM_CHARTS_REPO_URL` (the operator repo, for the `charts/tenant`
boundary chart). Register the EKS cluster (SlimConfig: API endpoint + CA + a
`view`-bound SA token; add the AWS provider so the connection-test assume-role
runs). Then drive create-tenant in the UI → it renders `charts/tenant`
(Platform + BudgetPolicy) → commits to the tenants repo → ArgoCD applies → the
operator provisions namespace + real per-Platform IRSA + KMS. The app workloads
land via the eks-gitops `apps-tenants` ApplicationSet, with per-env values from
`tofu output` (platform role ARN, tenant infra).

> Portal-driven deploy is the one path that was NOT live-exercised on EKS in the
> 2026-05-31 run — verify it on first use. Everything below it (operator real
> reconcile) is verified.

### B4. Validate

```bash
<cloudgov> platform audit --kubeconfig ~/.kube/config   # 0 findings (k8s + AWS IRSA)
```

Spot-check: tenant IAM role exists at `…:role/eks-agent-platform/tenants/<env>-<platform>-tenant`
with the permissions boundary attached and a trust policy scoped to exactly
`system:serviceaccount:tenants-<platform>:tenant-runtime`; tenant `/readyz` green.

### B5. Teardown (reverse order — stops spend)

1. **Delete the Platform CRs first** so the operator finalizer reaps the
   operator-created tenant IAM roles (they're outside Terraform state — if you
   skip this they orphan). Confirm `aws iam get-role` → NoSuchEntity.
2. `terragrunt destroy` each `<app>-platform`, then `agent-iam`, then
   `cluster-bootstrap` (uninstalls cilium/argocd — do it while the cluster's up),
   then `cluster`, then `network`.
3. Confirm zero billable resources: `aws eks list-clusters`,
   `aws ec2 describe-nat-gateways --filter Name=state,Values=available`,
   `aws ec2 describe-vpcs --filters Name=tag:Project,Values=landing-zone`.
4. Revert the local `account.hcl` back to the placeholder.

---

## Gotchas (fixed-forward, or watch for)

- **cilium + NetworkPolicy:** the operator (and any apiserver-egress workload)
  needs `networkPolicy.engine=cilium`; a vanilla NetworkPolicy can't allow
  kube-apiserver egress under cilium. (Fixed in the chart.)
- **EKS image arch:** Graviton nodes need an arm64 image. The Docker _default_
  driver silently ignores buildx `TARGETARCH`, so a local
  `docker buildx build --platform linux/arm64` can still produce amd64 — pass
  `--build-arg TARGETARCH=arm64 --no-cache --provenance=false` (the provenance
  attestation manifest also confuses kubelet platform selection). The
  `release.yaml` CI build is already multi-arch, so a published tag avoids this.
- **imagePullPolicy:** `IfNotPresent` + an unchanged tag caches the old image on
  the node — bump the tag or force a re-pull when iterating.
- **EKS kube-version:** the operator chart's `kubeVersion` allows the EKS
  pre-release suffix (`v1.35.x-eks-…`). (Fixed.)
- **Upstream chart value schemas:** don't inject `clusterName`/`vpcId` (or other
  unknown `--set` values) into upstream charts with strict schemas (cert-manager,
  kagent, agentgateway) — they fail to render. (Fixed in the ApplicationSets.)
- **Operator IAM idempotency GetRole:** authorizes against the bare-name (root)
  ARN before the role exists, so the operator role allows GetRole on the
  `<env>-*-tenant` name pattern as well as the scoped path. (In `agent-iam`.)
