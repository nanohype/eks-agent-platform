# charts/operator

Helm chart for the eks-agent-platform operator: CRDs + Deployment + RBAC + Service + ServiceMonitor + NetworkPolicy + PDB, plus the eval-runtime (Argo WorkflowTemplate/AnalysisTemplate) behind `evalRuntime.*`.

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

- `serviceAccount.annotations."eks.amazonaws.com/role-arn"` — required; the operator IRSA role from `landing-zone/components/aws/agent-iam`
- `reconcilers.budget.requeueInterval` — production: 1h, dev: 5m
- `metrics.secure` — **default true**: serve metrics over HTTPS and require each scrape to authenticate + authorize (controller-runtime's authn/authz filter — TokenReview + a SubjectAccessReview on `/metrics`), so the endpoint rejects unauthenticated scrapes at the HTTP layer, not just via NetworkPolicy. The chart wires the operator SA for token review and a metrics-reader SA + token the ServiceMonitor presents. A plaintext annotation-based scraper cannot read the endpoint while secure, so the `prometheus.io/scrape` pod annotations are suppressed; scrape via the authenticated ServiceMonitor or an agent that presents the metrics-reader token over https. Set false only where no kube-apiserver is reachable to review tokens.
- `metrics.serviceMonitor.enabled` — requires Prometheus operator CRDs (from `eks-gitops`)

### eval-runtime (`evalRuntime.*`)

The Argo Workflows runtime the operator submits EvalSuite runs to (WorkflowTemplate `eval-runner` + the gating AnalysisTemplate + SA/RBAC). Enabled by default; needs the Argo Workflows CRD.

- `evalRuntime.namespace` / `serviceAccount.name` — byte-pinned to the `aws_eks_pod_identity_association` in `terraform/components/eval-runtime` (`eval-runner`/`eval-runner`), which binds the role to that exact namespace/SA pair; change both together or the association matches nothing and the runner gets no credentials
- No role ARN is passed to this chart. Pod Identity binds the role to the ServiceAccount in AWS, so nothing needs injecting into the manifest — there is no `evalRuntime.serviceAccount.roleArn` value and the ApplicationSet sets none
- `evalRuntime.evalReportsBucket` — S3 bucket for eval reports (terraform output, injected per-cluster)
- `evalRuntime.rollouts.enabled` — the AnalysisTemplate; **off by default** (needs the Argo Rollouts CRD)

### operator SLO — not in this chart

There are no `slo.*` values. The PrometheusRule, the AlertmanagerConfig and the
namespace that held them were removed at chart 0.6.0, and `values.schema.json`
now closes the top level, so setting one is not merely ignored — `helm template
--set slo.enabled=true` fails with `additional properties 'slo' not allowed`.

The operator's alert rules live in eks-gitops as Grafana-managed rules
(`dashboards/base/alerting/agent-operator.yaml`), evaluated against AMP rather
than by a prometheus-operator in-cluster. The `slo:` key that does exist here is
`reconcilers.slo`, which toggles the SLOPolicy reconciler in the binary and is
unrelated.

## Required cluster capabilities

- Kubernetes 1.32+ (DRA + structured authentication config)
- Prometheus operator CRDs (for ServiceMonitor)
- ArgoCD CRDs (`AppProject`) — operator reconciles these for tenant scoping

All three are provided by `eks-gitops`.
