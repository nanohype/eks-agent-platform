# eks-agent-platform

![Kubernetes](https://img.shields.io/badge/Kubernetes-Native-326CE5?logo=kubernetes)
![EKS](https://img.shields.io/badge/AWS-EKS-FF9900?logo=amazonaws)
![Bedrock](https://img.shields.io/badge/AWS-Bedrock-FF9900?logo=amazonaws)
![OpenTofu](https://img.shields.io/badge/OpenTofu-%3E%3D1.11-blue?logo=opentofu)
![ArgoCD](https://img.shields.io/badge/ArgoCD-GitOps-EF7B4D?logo=argo)
![License](https://img.shields.io/badge/License-Apache--2.0-green)

A Kubernetes-native, AWS-native **platform-of-platforms**. Each team's agent workloads are declared as a `Tenant` CR; the operator provisions the per-tenant IRSA, KMS grants, S3 prefixes, agentgateway routes, kagent runtime, KEDA scaling, budget kill-switch, and Argo-Workflows eval pipeline. Eight personas (sales-ops, support, finance, ops, founder, eng, marketing, legal) are first-class users with their own onboarding playbooks + agentctl scaffolding.

**AI clients / agents start here:** [`AGENTS.md`](AGENTS.md). For the stack-wide view, see the [Platform Reference](https://github.com/nanohype/nanohype/blob/main/docs/platform-reference.md).

Bedrock for model access, [kagent](https://www.cncf.io/projects/kagent/) for the agent runtime, [agentgateway](https://agentgateway.dev/) for the model/tool data plane, [DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) for accelerator scheduling.

Sits on top of [landing-zone](https://github.com/nanohype/landing-zone) (Terragrunt org/account/cluster scaffolding) and [eks-gitops](https://github.com/nanohype/eks-gitops) (general-purpose ArgoCD addons).

## 60 seconds — what's here

| Persona                              | Start here                                                                                                                                   |
| ------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------- |
| You're an engineer onboarding a team | [`docs/onboarding/eng.md`](./docs/onboarding/eng.md)                                                                                         |
| You're a non-eng team lead           | pick your role in [`docs/onboarding/`](./docs/onboarding/) — playbooks for sales-ops / support / finance / ops / founder / marketing / legal |
| You're SRE on-call                   | [`docs/runbooks/`](./docs/runbooks/) — alert-driven + scenario-driven                                                                        |
| You want the architecture            | [`docs/architecture/`](./docs/architecture/) — overview + flow diagrams + multi-cluster, plus [`docs/adr/`](./docs/adr/)                     |
| You're picking apart the CRDs        | browsable index at [`docs/crd-reference/`](./docs/crd-reference/) (regenerated from godoc on every `make manifests`)                         |
| You want to see the model in action  | [`examples/blank-tenant/`](./examples/blank-tenant/) — minimum-viable Platform CR set + smoke-test eval                                      |

## Layout

| Layer        | What's in it                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| ------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `terraform/` | OpenTofu/Terragrunt components: `bedrock` (invocation logging + Guardrails), `agent-egress` (PrivateLink + WAF), `accelerator-pools` (NVIDIA + Neuron), `kill-switch` (EventBridge + Step Functions), `cost-pipeline` (CUR + Athena + Glue Crawler + invocation-cost-publisher Lambda), `eval-runtime` (eval-runner IRSA + Workflow infra). The model-artifacts and eval-reports buckets are provisioned by landing-zone's `agent-iam` component (the sole writer of the `/eks-agent-platform/<cluster>/model-artifacts/` SSM contract these components read). |
| `operators/` | Go (kubebuilder v4) — one binary, nine reconcilers (`tenant`, `platform`, `gateway`, `runtime`, `budget`, `eval`, `sandboxpool`, `agentsandbox`, `batch`), one shared leader-election lease. Owns per-tenant AWS state via in-cluster IRSA. Also ships `agentctl` CLI.                                                                                                                                                                                                                                                                                         |
| `charts/`    | Helm — `operator` (CRDs + Deployment + RBAC + cert-manager-issued webhook cert; ships its own eval-runtime and SLO bundles behind `evalRuntime.*` / `slo.*` toggles), `bedrock-egress`, `tenant` (opinionated `Platform` CR scaffold).                                                                                                                                                                                                                                                                                                                         |
| `examples/`  | `blank-tenant` (smoke-test single-agent Platform), `agent-fleet` (KEDA + ToolServer snippet), `bedrock-rag` (RAG snippet).                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `docs/`      | `onboarding/` (per-persona playbooks), `runbooks/` (alert + scenario playbooks), `architecture/` (overview + flow diagrams + multi-cluster), `adr/` (Architecture Decision Records), `crd-reference/` (CRD index).                                                                                                                                                                                                                                                                                                                                             |

## CRDs

Split across three capability groups under the `nanohype.dev` domain (version `v1alpha1`): `platform.nanohype.dev` (Tenant, Platform), `agents.nanohype.dev` (AgentFleet, ModelGateway, AgentSandbox, SandboxPool, BatchJob), `governance.nanohype.dev` (BudgetPolicy, EvalSuite). Composed on top of kagent's `Agent`/`ModelConfig`/`ToolServer`.

| Kind           | Scope      | Owns                                                                                     |
| -------------- | ---------- | ---------------------------------------------------------------------------------------- |
| `Tenant`       | Cluster    | Aggregate budget + readiness + suspension across a tenant's Platforms                    |
| `Platform`     | Namespaced | Tenant workload namespace, IRSA role, KMS grant, S3 bucket policy, ArgoCD AppProject     |
| `ModelGateway` | Namespaced | agentgateway Route per ModelRoute (Bedrock backend + Guardrail attachment)               |
| `AgentFleet`   | Namespaced | kagent Agent + ModelConfig per agent, KEDA ScaledObject (SQS or CPU), NetworkPolicy      |
| `BudgetPolicy` | Namespaced | Hourly Athena CUR aggregation + CloudWatch in-flight estimate; kill-switch event at 120% |
| `EvalSuite`    | Namespaced | Argo Workflow/CronWorkflow against the fleet; status writeback by the runner template    |

## Quickstart

```bash
# Prereqs: tofu >=1.11, terragrunt, kubectl, helm, argocd CLI, pnpm >=11, go >=1.26
git clone git@github.com:nanohype/eks-agent-platform.git
cd eks-agent-platform
pnpm install
task --list

# Validate everything locally
task ci

# Substrate (per environment)
task tofu:apply ENVIRONMENT=dev COMPONENT=bedrock
task tofu:apply ENVIRONMENT=dev COMPONENT=cost-pipeline
task tofu:apply ENVIRONMENT=dev COMPONENT=kill-switch
task tofu:apply ENVIRONMENT=dev COMPONENT=eval-runtime

# Cluster-side delivery (operator + agent addons) lives in eks-gitops —
# addons-agent-operator git-sources charts/operator and injects per-cluster IRSA.

# Onboard a tenant (persona-flexed scaffolding)
agentctl tenant init my-team --persona support --slack '#my-team' \
  | kubectl apply -f -
agentctl tenant get my-team
```

### Bootstrap note (first-time setup)

The operator chart is pulled from `oci://ghcr.io/nanohype/eks-agent-platform/charts/operator`. On a fresh fork the OCI registry is empty until you cut the first `charts-v*` release tag. Until then:

```bash
helm install operator ./charts/operator \
  -n eks-agent-platform --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="$(aws ssm get-parameter --name /eks-agent-platform/dev/agent-iam/operator_role_arn --query Parameter.Value --output text)" \
  --set config.environment=dev
```

Or cut a release: `git tag charts-v0.1.0 && git push origin charts-v0.1.0` (triggers `.github/workflows/release.yaml`).

## What happens when a tenant breaches budget

1. `BudgetReconciler` ticks hourly, queries the CUR Athena table + CloudWatch in-flight metric, computes percent-of-budget.
2. At ≥ 120% with `KillSwitchEnabled: true`, the reconciler publishes a `BudgetBreach` event to the kill-switch EventBridge bus.
3. The kill-switch Step Functions state machine detaches the baseline policy from the tenant IRSA role AND tags the role with `platform.nanohype.dev/suspended=true`.
4. On its next reconcile (≤60s), the operator's `PlatformReconciler` sees the suspension tag, sets `Platform.status.phase = Suspended`, and `AgentFleetReconciler` tears down kagent Agents + KEDA ScaledObject so no pods can serve traffic.
5. Slack #incidents + PagerDuty fire (`PlatformSuspended` alert from `operator-slo`).
6. Recovery: ops removes the IAM tag; next reconcile sees the cleared tag, reattaches the baseline, fleet scales back up. No CR mutation required.

Full sequence + recovery in [`docs/runbooks/platform-suspended.md`](./docs/runbooks/platform-suspended.md). Threat model: [`docs/adr/0003-threat-model.md`](./docs/adr/0003-threat-model.md).

## Boundaries

This repo **builds the product**: the operator (`charts/operator` — CRDs, Deployment, RBAC, plus the eval-runtime and SLO bundles behind chart toggles) and the per-tenant AWS state (`terraform/`). It is not a deploy catalog.

Cluster delivery lives in [`eks-gitops`](https://github.com/nanohype/eks-gitops):

- `addons-agent-operator` git-sources `charts/operator` and injects per-cluster IRSA/OIDC (operator role, eval-runner role ARN, report bucket) from the cluster-Secret annotations `cluster-bootstrap` sets.
- `addons-ai-platform` delivers kagent + agentgateway.
- `addons-argo-platform` delivers Argo Workflows + Rollouts + Events.
- `addons-accelerators-{helm,kustomize}` deliver the GPU operator, NVIDIA DRA driver, and AWS Neuron device plugin.

Clusters opt in via the label `eks-agent-platform/enabled=true`.

It also deliberately does **not** own:

- Org, account, network, EKS cluster, baseline IAM → [`landing-zone`](https://github.com/nanohype/landing-zone)
- General-purpose cluster addons (cert-manager, cilium, kyverno, observability stack) → [`eks-gitops`](https://github.com/nanohype/eks-gitops)
- Cluster bootstrap (ArgoCD install, app-of-apps wiring) → `landing-zone` (OpenTofu)

## Contributing

Conventional commits enforced via commitlint. `task ci` runs the full lint + test matrix locally. See [`CONTRIBUTING.md`](./CONTRIBUTING.md).

## License

Apache-2.0.
