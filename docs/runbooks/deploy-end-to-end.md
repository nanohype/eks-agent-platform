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
3. `managed-monitoring` — the AMP + AMG workspaces. Publishes the AMP endpoints
   to Secrets Manager (External Secrets syncs them to the Grafana data source and
   grafana-agent) and the AMG workspace URL to SSM. `cluster-bootstrap` reads that
   URL (its `enable_managed_monitoring` is on by default) to annotate the cluster
   Secret, so apply this before `cluster-bootstrap`.
4. `cluster-bootstrap` — installs cilium (ENI mode, replaces VPC-CNI) + ArgoCD +
   an app-of-apps pointing at `eks-gitops/applicationsets`. It also registers the
   cluster as an ArgoCD cluster Secret carrying the `eks-agent-platform/enabled=true`
   label, the operator-role-arn annotation the operator ApplicationSet reads, and
   the `monitoring/grafana-url` annotation the dashboards ApplicationSet injects
   into the Grafana CR. ArgoCD then syncs the addon catalog onto every labeled
   cluster: `addons-ai-platform` (kagent + agentgateway), `addons-argo-platform`
   (argo-workflows/rollouts/events), the `accelerators` category (gpu-operator,
   NVIDIA DRA driver, AWS Neuron device plugin), and `addons-agent-operator` (the
   operator itself).
5. `agent-iam` — the operator IRSA role (path-scoped, boundary-gated), the tenant
   permissions boundary + baseline policies, and the SSM params the operator
   reads (`/eks-agent-platform/<env>/agent-iam/*`).
6. Per tenant, `<app>-platform` (e.g. `competitive-intelligence-platform`) for
   Aurora / per-tenant IRSA / Secrets. OIDC is wired from the `cluster`
   dependency automatically.

Confirm addons converge: `kubectl --context <cluster> get applications -n argocd`
(cert-manager, external-secrets, cilium, kagent, agentgateway, argo-workflows,
gpu-operator, nvidia-dra-driver, neuron-device-plugin, the operator + its
eval-runtime, … Synced/Healthy).

### B1b. Agent-platform AWS substrate (`eks-agent-platform/terraform`)

Apply once `agent-iam` (B1 step 5) and the cluster exist. This tree is the
operator's own AWS substrate — `bedrock`, `agent-egress`, `accelerator-pools`,
`eval-runtime`, `cost-pipeline`, `kill-switch`, `batch-runtime` — and writes the
SSM parameters the operator loads at startup
(`/eks-agent-platform/<env>/{bedrock,kill-switch,cost-pipeline,eval-runtime,
batch-runtime}/*`). `agent-iam` is **not** here — landing-zone owns it (B1 step
5), and it is also the sole owner of the model-artifacts + eval-reports buckets
and their `/eks-agent-platform/<env>/model-artifacts/*` SSM keys; this tree reads
the operator role, tenant baseline, and the eval-reports/model-artifacts buckets
from that landing-zone SSM contract.

**Prereq:** the `eks-pod-identity-agent` EKS addon must be installed — the
accelerator (neuron, gpu-operator) and eval-runner roles bind via EKS Pod
Identity, which is inert without it. Check: `aws eks describe-addon
--cluster-name <cluster> --addon-name eks-pod-identity-agent`.

From `eks-agent-platform/`, export `AWS_ACCOUNT_ID` (it names the state bucket)
and the `TF_VAR_*` infrastructure identifiers the orchestrator supplies —
`TF_VAR_data_kms_key_arn`, `TF_VAR_logs_kms_key_arn` (lz-secrets); the
agent-egress VPC / subnet / route-table / security-group IDs (lz-network /
lz-cluster); `TF_VAR_node_role_name` (lz-cluster, changes on cluster recreate) —
then apply in dependency order:

```bash
task tofu:apply ENVIRONMENT=<env> COMPONENT=all   # terragrunt run --all resolves the graph
```

Verify the Pod Identity associations bound:
`aws eks list-pod-identity-associations --cluster-name <cluster>` should list the
`aws-neuron/neuron-device-plugin`, `gpu-operator/gpu-operator`, and
`eval-runner/eval-runner` tuples. From one of those pods,
`AWS_CONTAINER_CREDENTIALS_FULL_URI` is injected (Pod Identity), not
`AWS_ROLE_ARN`/`AWS_WEB_IDENTITY_TOKEN_FILE` (IRSA), and `aws sts
get-caller-identity` resolves to the bound role. A `(namespace, serviceAccount)`
mismatch against the running chart is a silent no-credentials failure — confirm
the chart-rendered SA names before assuming a deeper problem.

### B2. Operator on EKS

The operator syncs itself. The `addons-agent-operator` ApplicationSet in
eks-gitops git-sources `charts/operator` and targets every cluster carrying
`eks-agent-platform/enabled=true`. It injects the per-cluster bits the chart
can't hardcode — the operator IAM role ARN and the eval-reports bucket — from
the annotations `cluster-bootstrap` publishes on the ArgoCD cluster Secret (the
eval-runner role is bound by Pod Identity, see B1b). The cluster name comes from
SSM
(`/eks-agent-platform/<env>/cluster/name`), read by the operator's config
loader. So once B1 step 3
landed, the operator (with the AWS reconcile ON and its eval-runtime + SLO
bundles enabled by the chart defaults) is already on its way. Get the image
published so the ApplicationSet has something to pull:

- **Published (preferred):** push a release tag in `eks-agent-platform` →
  `release.yaml` builds a multi-arch image to
  `ghcr.io/nanohype/eks-agent-platform/operator:<version>`.
- **Dev/in-account ECR:** build for the node arch and push to ECR — see the
  arch gotcha below.

The operator loads the rest of its config (tenant IAM path, baseline +
boundary ARNs) from SSM via its IRSA role. Verify:
`kubectl --context <cluster> logs -n eks-agent-platform -l app.kubernetes.io/name=operator`
→ "AWS substrate loaded".

**First-boot fallback.** If you need the operator up before the ApplicationSet
reconciles (or you're debugging outside ArgoCD), install the chart by hand with
the same per-cluster values:

```bash
helm upgrade --install eks-agent-platform eks-agent-platform/charts/operator \
  --kube-context <cluster> -n eks-agent-platform --create-namespace \
  --set image.repository=<registry>/eks-agent-platform/operator \
  --set image.tag=<version> \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=<agent-iam operator role ARN> \
  --set config.environment=<env> --set config.region=<r> \
  --set config.clusterName=<cluster> \
  --set evalRuntime.serviceAccount.roleArn=<eval-runner role ARN> \
  --set evalRuntime.evalReportsBucket=<eval-reports bucket> \
  --set networkPolicy.engine=cilium --wait
```

### B3. Deploy tenants through the portal

```bash
cd portal && docker compose up -d && task dev
```

Config the worker (env): `GITOPS_TENANTS_REPO_URL` (the tenants gitops repo),
`GITOPS_TENANTS_REPO_REF`, `GITOPS_SSH_KEY_PATH`, `GITOPS_AUTHOR_NAME/EMAIL`,
`EKS_AGENT_PLATFORM_CHARTS_REPO_URL` (the operator repo, for the `charts/tenant`
boundary chart). **`GITOPS_TENANTS_REPO_URL` must equal the `repoURL` in the
eks-gitops `portal-tenants` ApplicationSet** — that's what applies the commits.
Register the EKS cluster (SlimConfig: API endpoint + CA + a `view`-bound SA
token; add the AWS provider so the connection-test assume-role runs). Then drive
create-tenant in the UI → it renders `charts/tenant` (Tenant + Platform +
BudgetPolicy + agent-plane CRs) → commits to `tenants/<cluster>/<tenant>.yaml` →
the `portal-tenants` ApplicationSet applies it → the operator provisions the
namespace + real per-Platform IRSA + KMS → the watcher lists the Tenant back into
the portal inventory. The app workloads land separately via the `apps-tenants`
ApplicationSet, with per-env values from `tofu output`.

> The write→read loop (render → commit → apply → reconcile → watcher observes) was
> validated on kx on 2026-05-31: the rendered Tenant + Platform reconcile to Ready
> and the Tenant appears under the watcher's `tenants.platform.nanohype.dev` query,
> cloudgov-clean. NOT yet exercised on real EKS: the full portal stack
> (server/worker/DB) + the `portal-tenants` ApplicationSet's ArgoCD apply — verify
> those on first EKS use. (On kx, ArgoCD is idle by design, so the rehearsal applied
> the rendered manifest directly.)

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
