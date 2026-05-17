# charts/operator

Helm chart for the eks-agent-platform operator: CRDs + Deployment + RBAC + Service + ServiceMonitor + NetworkPolicy + PDB.

## Install

```bash
# OCI (once published)
helm install operator oci://ghcr.io/stxkxs/eks-agent-platform/charts/operator \
  --version 0.1.0 \
  --namespace eks-agent-platform --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::<acct>:role/<env>-<cluster>-eap-operator" \
  --set config.environment=dev \
  --set config.region=us-west-2

# Local
helm install operator ./charts/operator -n eks-agent-platform --create-namespace
```

## CRDs

CRDs are bundled in `crds/` and populated by `make manifests` in `operators/`. Helm install/upgrade does **not** modify existing CRDs (this is Helm's default; safe for re-installs). Use the `chart` CLI helper if you need to upgrade CRDs in place: `helm upgrade --install operator ... --set crds.upgrade=true`.

## Values

See [`values.yaml`](./values.yaml). Highlights:

- `serviceAccount.annotations."eks.amazonaws.com/role-arn"` — required; the operator IRSA role from `terraform/components/agent-iam`
- `reconcilers.budget.requeueInterval` — production: 1h, dev: 5m
- `webhooks.enabled` — set false only for development; production requires admission webhooks
- `metrics.serviceMonitor.enabled` — requires Prometheus operator CRDs (from `eks-gitops`)

## Required cluster capabilities

- Kubernetes 1.32+ (DRA + structured authentication config)
- cert-manager (for webhook serving cert)
- Prometheus operator CRDs (for ServiceMonitor)
- ArgoCD CRDs (`AppProject`) — operator reconciles these for tenant scoping

All four are provided by `eks-gitops`.
