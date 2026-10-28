# charts/operator

Helm chart for the eks-agent-platform operator: CRDs + Deployment + RBAC + Service + ServiceMonitor + NetworkPolicy + PDB, plus the eval-runtime (Argo WorkflowTemplate/AnalysisTemplate) behind `evalRuntime.*`.

## Install

```bash
# OCI (once published)
helm install operator oci://ghcr.io/nanohype/eks-agent-platform/charts/operator \
  --version 0.6.8 \
  --namespace eks-agent-platform --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::<acct>:role/<env>-<cluster>-eks-agent-platform-operator" \
  --set config.environment=dev \
  --set config.region=us-west-2

# Local
helm install operator ./charts/operator -n eks-agent-platform --create-namespace
```

## CRDs

CRDs are bundled in `crds/` and populated by `make manifests` in `operators/`. Helm install/upgrade does **not** modify existing CRDs (this is Helm's default; safe for re-installs). To roll a CRD change, apply `crds/` directly: `kubectl apply -f charts/operator/crds/`.

## Values

See [`values.yaml`](./values.yaml). Highlights:

- `serviceAccount.annotations."eks.amazonaws.com/role-arn"` — required; the operator IRSA role from `landing-zone/components/aws/agent-iam`
- `reconcilers.budget.requeueInterval` — production: 1h, dev: 5m
- `metrics.secure` — **default true**: serve metrics over HTTPS and require each scrape to authenticate + authorize (controller-runtime's authn/authz filter — TokenReview + a SubjectAccessReview on `/metrics`), so the endpoint rejects unauthenticated scrapes at the HTTP layer, not just via NetworkPolicy. The chart wires the operator SA for token review and a metrics-reader SA + token the ServiceMonitor presents. A plaintext annotation-based scraper cannot read the endpoint while secure, so the `prometheus.io/scrape` pod annotations are suppressed; scrape via the authenticated ServiceMonitor or an agent that presents the metrics-reader token over https. Set false only where no kube-apiserver is reachable to review tokens.
- `metrics.serviceMonitor.enabled` — requires Prometheus operator CRDs (from `eks-gitops`)

### eval-runtime (`evalRuntime.*`)

The Argo Workflows runtime the operator submits EvalSuite runs to (WorkflowTemplate `eval-runner` + the gating AnalysisTemplate + the RBAC its runs hold). Enabled by default; needs the Argo Workflows CRD.

Runs hold no shared AWS identity. The operator creates one ServiceAccount per Platform in the eval runtime namespace (`eval-<platform>`), binds it by EKS Pod Identity to a `<cluster>-<platform>-eval` role scoped to that Platform's prefixes in the eval-reports bucket, and submits each run under it once the Platform reports `EvalIdentityReady=True`. The chart creates no ServiceAccount and takes no role ARN for them.

The chart's run RBAC is the Kubernetes half, and it is shared by every Platform's runs. A ClusterRoleBinding gives the eval runtime namespace's ServiceAccount group `get` on `evalsuites` and `get`/`patch` on `evalsuites/status` in every namespace, which is what writeback and the exit handler call; a RoleBinding gives the same group `create`/`patch` on `workflowtaskresults` in the eval runtime namespace for Argo's executor. So any run can read any EvalSuite and patch any suite's status, and nothing on the Kubernetes side keeps one Platform's run off another Platform's score. A grant narrowed to one suite's status would not change that: a step's outputs, the score among them, travel to the next step through `workflowtaskresults`, which Argo's executor writes namespace-wide. What separates Platforms is the IAM role, inside the bucket they share. A run cannot create a Workflow, so it cannot start one under another Platform's account.

- `evalRuntime.namespace` — must equal the `runner_namespace` that `terraform/components/eval-runtime` publishes to SSM: the operator submits runs and creates each Platform's eval ServiceAccount in the namespace it reads there, and this chart installs the WorkflowTemplate and the run RBAC in this one
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

- Kubernetes 1.32+ (structured authentication config)
- Prometheus operator CRDs (for ServiceMonitor)
- ArgoCD CRDs (`AppProject`) — operator reconciles these for tenant scoping

All three are provided by `eks-gitops`.
