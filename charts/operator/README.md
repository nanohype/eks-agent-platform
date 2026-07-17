# charts/operator

Helm chart for the eks-agent-platform operator: CRDs + Deployment + RBAC + Service + ServiceMonitor + NetworkPolicy + PDB, plus the operator's own runtime — the eval-runtime (Argo WorkflowTemplate/AnalysisTemplate) and SLO (PrometheusRule/AlertmanagerConfig/CR-state metrics), behind `evalRuntime.*` / `slo.*` toggles.

## Install

```bash
# OCI (once published)
helm install operator oci://ghcr.io/nanohype/eks-agent-platform/charts/operator \
  --version 0.2.0 \
  --namespace eks-agent-platform --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::<acct>:role/<env>-<cluster>-eks-agent-platform-operator" \
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
- `metrics.serviceMonitor.enabled` — requires Prometheus operator CRDs (from `eks-gitops`)

### eval-runtime (`evalRuntime.*`)

The Argo Workflows runtime the operator submits EvalSuite runs to (WorkflowTemplate `eval-runner` + the gating AnalysisTemplate + SA/RBAC). Enabled by default; needs the Argo Workflows CRD.

- `evalRuntime.namespace` / `serviceAccount.name` — byte-pinned to the `terraform/components/eval-runtime` IRSA trust (`eval-runner`/`eval-runner`); change both together or IRSA breaks
- `evalRuntime.serviceAccount.roleArn` — embeds the AWS account id, so it is injected per-cluster by the eks-gitops `addons-agent-operator` ApplicationSet from the `eks-agent-platform/eval-runner-role-arn` cluster-Secret annotation, never committed here
- `evalRuntime.evalReportsBucket` — S3 bucket for eval reports (terraform output, injected per-cluster)
- `evalRuntime.rollouts.enabled` — the AnalysisTemplate; **off by default** (needs the Argo Rollouts CRD)

### operator SLO (`slo.*`)

The operator's own observability: recording rules + alerts and persona alert routing. Enabled by default; needs the prometheus-operator CRDs. The kube-state-metrics CustomResourceState config that makes the `kube_customresource_*` metrics exist lives in the eks-gitops kube-state-metrics addon (the single source — it's what KSM actually loads); this chart consumes those metrics, it doesn't define them.

- `slo.operatorNamespace` — the namespace the operator runs in, used in the PromQL metric selectors
- `slo.alerting.enabled` — the AlertmanagerConfig persona routing; **off by default** — its receivers reference six Secrets (`pagerduty-platform`, `slack-webhook-{incidents,finance,ops,eng,platform}`) that must pre-exist; enable per-env in production once provisioned

## Required cluster capabilities

- Kubernetes 1.32+ (DRA + structured authentication config)
- Prometheus operator CRDs (for ServiceMonitor)
- ArgoCD CRDs (`AppProject`) — operator reconciles these for tenant scoping

All three are provided by `eks-gitops`.
