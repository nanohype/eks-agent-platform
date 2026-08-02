# Onboarding — Local kx cluster

Land the eks-agent-platform operator + a smoke-test tenant on the [`kx`](https://github.com/nanohype/kx) kind cluster. Two modes:

- **k8s-only** — operator runs `--disable-aws`; validates the CR-emission paths (Platform → tenant ns + Gateway + agent Deployment + KEDA ScaledObject) without touching AWS. Useful when you're iterating on the operator binary or debugging upstream CRD version drift.
- **bedrock** — also gives the tenant gateways an AWS identity, so the full loop works end-to-end (tenant pod → gateway → Bedrock → response). Real model calls, real cost.

## Prereqs

kx already ships every upstream we need. Enable the slices first:

```bash
cd ../kx
task stack:ai-platform:enable     # Envoy AI Gateway
task stack:autoscaling:enable     # KEDA
task stack:argo-platform:enable   # argo-workflows + argo-rollouts
task stack:security:enable        # Kyverno — only needed for --with-bedrock
```

Verify the CRDs landed:

```bash
kubectl get crd aigatewayroutes.aigateway.envoyproxy.io gateways.gateway.networking.k8s.io scaledobjects.keda.sh workflows.argoproj.io
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

kubectl get -n tenants-blank aigatewayroutes.aigateway.envoyproxy.io -l 'agents.nanohype.dev/platform=blank'
# blank-primary route present

kubectl get -n tenants-blank deploy scaledobjects.keda.sh
# an agent Deployment per AgentSpec + the KEDA ScaledObject targeting it
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

This delegates to kx's `bedrock-credentials` slice, which needs kx's security slice for Kyverno. What it adds on top of the k8s-only flow:

1. Resolves AWS credentials from `${AWS_PROFILE:-default}` via `aws configure export-credentials`, which turns SSO and role-chained sessions into static ones.
2. Stores them once, where Kyverno can clone them.
3. Installs a policy that clones the Secret into each `tenants-*` namespace as it appears, and adds it to that namespace's gateway `envoy` container.

**Why a policy rather than a patch.** On a real cluster the gateway reaches Bedrock as the tenant: the operator points each Platform's EnvoyProxy at the tenant ServiceAccount, which carries a Pod Identity association, and the `BackendSecurityPolicy` names only a region — the ambient credential chain. kind has no Pod Identity, so that chain finds nothing.

Credentials cannot simply be written onto the pod, because the data plane is generated: Envoy Gateway renders the Deployment from the EnvoyProxy the operator renders, so anything patched onto either is reconciled away — it works just long enough to look correct. Mutating at admission reapplies itself every time the pod is recreated, and the operator is untouched.

The credentials land on the `envoy` container specifically, because Envoy AI Gateway signs AWS requests in Envoy itself rather than in a sidecar.

### Verify the Bedrock loop end-to-end

```bash
kubectl run -n tenants-blank curl --rm -it --image=curlimages/curl --restart=Never -- \
  curl -sX POST http://blank-gateway.tenants-blank.svc.cluster.local:8080/anthropic/v1/messages \
       -H 'content-type: application/json' \
       -d '{"route":"blank-primary","messages":[{"role":"user","content":"ping"}],"max_tokens":16}'
```

Expected: an Anthropic message envelope with a real Bedrock-generated response.

### What this DOESN'T give you

- **No per-tenant identity.** kind has no Pod Identity, so the operator stays `--disable-aws` and mints no tenant IAM roles. Every tenant gateway shares one credential with whatever scope your laptop has — usually a lot — where a real cluster gives each tenant its own. Don't run anything sensitive against this cluster.
- **No KMS grants, no S3 bucket policies, no Athena CUR, no kill-switch.** The Budget reconciler reports zero spend; the kill-switch tag-detection flow is unexercised. Those paths are covered by `make -C operators test` (envtest) — kx is for the cluster-side wiring that envtest can't validate.
- **No ArgoCD-driven install.** kx's ArgoCD is idle by convention; we use `helm upgrade --install` directly. If you want to debug the ApplicationSet path locally, see [docs/architecture/multi-cluster.md](../architecture/multi-cluster.md).

## Tear down

```bash
./scripts/local-kx/uninstall.sh
```

Removes the operator, the blank tenant, the tenant workload namespace and the operator namespace. Leaves kx's upstream slices alone, along with any Bedrock credentials they installed — both belong to the workspace rather than to this install.

## Troubleshooting

| Symptom                                          | Cause / fix                                                                                                                                                                                                                                                                                                   |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Platform/blank` stays `Pending`                 | `kubectl describe platform blank -n eks-agent-platform` shows the failing step. Most common: a CRD wasn't installed (re-run `task stack:*:enable` in kx).                                                                                                                                                     |
| Bedrock `AccessDeniedException`                  | Either the credential lacks `bedrock:InvokeModel`, or it never reached the gateway. Check it reached first — `task -d ../kx stack:ai-platform:credentials:verify` — since a policy that matched nothing looks identical to a permissions problem from here. Then cross-check the identity with `aws sts get-caller-identity`. |
| Bedrock `ResourceNotFoundException` on the model | Your account doesn't have access to `claude-3-5-sonnet-20241022-v2:0` (the route's model). Either request access in the AWS console → Bedrock → Model access, or edit `examples/blank-tenant/platform.yaml`'s `ModelRouteSpec.modelId` to a model you do have.                                                |
| Operator pod crash-loops                         | `kubectl logs -n eks-agent-platform -l app.kubernetes.io/name=operator --tail=200`. Likely cause: a chart values knob that requires an integration we disabled (e.g. `metrics.serviceMonitor.enabled` without the Prometheus-operator CRDs, or `evalRuntime.rollouts.enabled` without the Argo Rollouts CRD). |

## Related

- [`docs/onboarding/eng.md`](./eng.md) — once the kx install proves out, the production onboarding is the same flow against a real EKS cluster.
- [`docs/architecture/overview.md`](../architecture/overview.md) — what the operator does once it's running.
- [kx README](https://github.com/nanohype/kx) — slice convention + cluster bootstrap.
