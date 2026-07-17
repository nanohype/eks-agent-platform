# Onboarding — Local kx cluster

Land the eks-agent-platform operator + a smoke-test tenant on the [`kx`](https://github.com/nanohype/kx) kind cluster. Two modes:

- **k8s-only** — operator runs `--disable-aws`; validates the CR-emission paths (Platform → tenant ns + agentgateway Route + kagent Agent + KEDA ScaledObject) without touching AWS. Useful when you're iterating on the operator binary or debugging upstream CRD version drift.
- **bedrock** — also mounts your laptop's AWS creds onto the agentgateway pod so the full loop works end-to-end (tenant pod → agentgateway → Bedrock → response). Real model calls, real cost.

## Prereqs

kx already ships every upstream we need. Enable the three slices first:

```bash
cd ../kx
task stack:ai-platform:enable     # kagent + agentgateway
task stack:autoscaling:enable     # KEDA
task stack:argo-platform:enable   # argo-workflows + argo-rollouts
```

Verify the CRDs landed:

```bash
kubectl get crd agents.kagent.dev routes.agentgateway.dev scaledobjects.keda.sh workflows.argoproj.io
```

`kx up` should already have given you `cert-manager`, `ArgoCD`, `prometheus-operator-crds` from `stack/core/`.

## k8s-only mode

From the eks-agent-platform repo root:

```bash
./scripts/local-kx/install.sh
```

What it does:

1. Confirms `kubectl current-context == kind-kx` (refuses otherwise).
2. Probes for the four required CRDs; prints which kx slice to enable if any are missing.
3. `helm upgrade --install operator ./charts/operator -n eks-agent-platform -f scripts/local-kx/values-local.yaml` — single replica, no leader election, `--disable-aws`.
4. Waits for the operator deployment to come up.
5. Applies `examples/blank-tenant/platform.yaml` — Platform + BudgetPolicy + ModelGateway + AgentFleet + EvalSuite for a single-agent smoke-test tenant.
6. Waits for `Platform/blank` to hit `NamespaceReady`.
7. Prints a summary with counts of emitted CRs.

### Verify

```bash
kubectl get platforms -A
# blank   ...   Ready

kubectl get -n tenants-blank ns,quota,limitrange,networkpolicy
# tenant ns + ResourceQuota + LimitRange + default-deny NetworkPolicy

kubectl get -n agentgateway routes.agentgateway.dev -l 'agents.nanohype.dev/platform=blank'
# blank-primary route present

kubectl get -n tenants-blank agents.kagent.dev modelconfigs.kagent.dev scaledobjects.keda.sh
# kagent Agent + ModelConfig + KEDA ScaledObject all emitted
```

If you built `bin/agentctl` (`make -C operators build-agentctl`):

```bash
./operators/bin/agentctl tenant list
./operators/bin/agentctl tenant get blank
```

## Bedrock mode

Adds Bedrock invocation capability on top of the k8s-only install.

```bash
./scripts/local-kx/install.sh --with-bedrock
```

What it adds on top of the k8s-only flow:

1. Resolves AWS credentials — first from `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` env vars if set, otherwise via `aws configure export-credentials --profile ${AWS_PROFILE:-default} --format env` (which resolves SSO + role-chained creds to static).
2. Creates a `agentgateway-aws` Secret in the `agentgateway` namespace with the resolved creds + `AWS_REGION`.
3. Patches the `agentgateway` Deployment to `envFrom` the Secret.
4. Rolls the deployment + waits for ready.

### Verify the Bedrock loop end-to-end

```bash
kubectl run -n tenants-blank curl --rm -it --image=curlimages/curl --restart=Never -- \
  curl -sX POST http://agentgateway.agentgateway.svc.cluster.local:8080/v1/messages \
       -H 'content-type: application/json' \
       -d '{"route":"blank-primary","messages":[{"role":"user","content":"ping"}],"max_tokens":16}'
```

Expected: an Anthropic message envelope with a real Bedrock-generated response.

### What this DOESN'T give you

- **No per-tenant IRSA.** kind has no OIDC provider; the operator stays `--disable-aws` so it doesn't try to mint tenant IAM roles. The agentgateway pod shares one cred with whatever scope your laptop has — usually a lot. Don't run anything sensitive against this cluster.
- **No KMS grants, no S3 bucket policies, no Athena CUR, no kill-switch.** The Budget reconciler reports zero spend; the kill-switch tag-detection flow is unexercised. Those paths are covered by `make -C operators test` (envtest) — kx is for the cluster-side wiring that envtest can't validate.
- **No ArgoCD-driven install.** kx's ArgoCD is idle by convention; we use `helm upgrade --install` directly. If you want to debug the ApplicationSet path locally, see [docs/architecture/multi-cluster.md](../architecture/multi-cluster.md).

## Tear down

```bash
./scripts/local-kx/uninstall.sh
```

Removes the operator, the blank tenant, the tenant workload namespace, the operator namespace, and the Bedrock-mode Secret. Leaves kx's upstream slices alone — they belong to kx.

## Troubleshooting

| Symptom                                          | Cause / fix                                                                                                                                                                                                                                                                                                   |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Platform/blank` stays `Pending`                 | `kubectl describe platform blank -n eks-agent-platform` shows the failing step. Most common: a CRD wasn't installed (re-run `task stack:*:enable` in kx).                                                                                                                                                     |
| `agents.kagent.dev` shape rejected               | kagent version drift between kx and our reconciler's emitted spec. Check `kubectl explain agent.spec` against `operators/internal/controller/agentfleet_reconcile.go:ensureKagentAgents`; bump kx's kagent chart version if needed.                                                                           |
| Bedrock `AccessDeniedException`                  | The static cred mounted on agentgateway doesn't have `bedrock:InvokeModel`. Confirm what the pod sees with `kubectl exec -n agentgateway deploy/agentgateway -- printenv AWS_ACCESS_KEY_ID` and cross-check with `aws sts get-caller-identity` for the same identity.                                         |
| Bedrock `ResourceNotFoundException` on the model | Your account doesn't have access to `claude-3-5-sonnet-20241022-v2:0` (the route's model). Either request access in the AWS console → Bedrock → Model access, or edit `examples/blank-tenant/platform.yaml`'s `ModelRouteSpec.modelId` to a model you do have.                                                |
| Operator pod crash-loops                         | `kubectl logs -n eks-agent-platform -l app.kubernetes.io/name=operator --tail=200`. Likely cause: a chart values knob that requires an integration we disabled (e.g. `metrics.serviceMonitor.enabled` without the Prometheus-operator CRDs, or `evalRuntime.rollouts.enabled` without the Argo Rollouts CRD). |

## Related

- [`docs/onboarding/eng.md`](./eng.md) — once the kx install proves out, the production onboarding is the same flow against a real EKS cluster.
- [`docs/architecture/overview.md`](../architecture/overview.md) — what the operator does once it's running.
- [kx README](https://github.com/nanohype/kx) — slice convention + cluster bootstrap.
