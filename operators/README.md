# operators/

The platform's Kubernetes operator. Single Go binary, six reconcilers, per-reconciler leader election. Built with kubebuilder v4 against controller-runtime v0.24.

## What it owns

| Reconciler | CR             | k8s-side state                                                                                                                                   | AWS-side state                                                                                                                    |
| ---------- | -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------- |
| `tenant`   | `Tenant`       | Aggregates Platform readiness/spend/suspension into `Tenant.status` for the per-tenant dashboard rollup                                          | —                                                                                                                                 |
| `platform` | `Platform`     | Tenant `Namespace` (with PSS label), `ResourceQuota`, `LimitRange`, default-deny `NetworkPolicy`, ArgoCD `AppProject`                            | IRSA role with trust policy bound to the tenant ServiceAccount, KMS grant on `cmk-data`, S3 bucket policy on the artifacts bucket |
| `gateway`  | `ModelGateway` | agentgateway `Route` per `ModelRoute`, with Bedrock backend + Guardrail attachment                                                               | —                                                                                                                                 |
| `runtime`  | `AgentFleet`   | kagent `Agent` + `ModelConfig` per agent, KEDA `ScaledObject` (SQS or CPU triggers), `NetworkPolicy`, tenant `ServiceAccount` annotated for IRSA | —                                                                                                                                 |
| `budget`   | `BudgetPolicy` | Status writeback: spend, percent-of-budget, conditions, alert thresholds crossed                                                                 | Athena `StartQueryExecution` against the CUR table, `CloudWatch:GetMetricData` for in-flight, `EventBridge:PutEvents` on breach   |
| `eval`     | `EvalSuite`    | Argo `CronWorkflow` + `WorkflowTemplate` reference, status writeback by the runner template                                                      | —                                                                                                                                 |

AWS-side reconciliation runs behind interface-injected clients (`IAM`/`KMS`/`S3`/`Athena`/`CloudWatch`/`EventBridge`) so the reconcilers stay unit-testable. In envtest, the clients are nil and the AWS paths short-circuit.

## How to build + run

```bash
make generate     # controller-gen → deepcopy + RBAC + CRDs
make manifests    # also regenerates docs/crd-reference/v1alpha1.md
make build        # bin/manager
make run          # against current kubecontext, leader-election off
make test         # envtest conformance suite + unit tests
make docker-build VERSION=0.1.0
```

The `manifests` target depends on `generate` and `crd-docs`, so a single invocation keeps the Helm chart's CRDs, the RBAC role, and the Markdown reference all consistent with the Go types.

## Layout

```
operators/
├── PROJECT                       # kubebuilder v4 metadata
├── api/v1alpha1/                 # CRD type definitions (source of truth)
│   ├── groupversion_info.go
│   ├── tenant_types.go
│   ├── platform_types.go
│   ├── modelgateway_types.go
│   ├── agentfleet_types.go
│   ├── budgetpolicy_types.go
│   └── evalsuite_types.go
├── internal/controller/          # reconcilers, split as `<kind>_controller.go` (Reconcile + setup) and `<kind>_reconcile.go` (the heavy lifting)
├── internal/awsclients/          # AWS SDK wrappers + interfaces for envtest stubbing
├── internal/agentctl/            # `agentctl` CLI library
├── cmd/                          # manager + agentctl entrypoints
├── config/                       # kustomize bases (generated)
├── test/conformance/             # envtest reconciler suite
├── hack/                         # boilerplate + crd-ref-docs config
├── Dockerfile                    # distroless, nonroot
├── Makefile
└── go.mod
```

## Design notes

See [`../ARCHITECTURE.md`](../ARCHITECTURE.md) for the bounded-context table, the operator-vs-OpenTofu split (operator owns fast-moving per-tenant AWS state via IRSA + AWS SDK; OpenTofu owns slow-moving platform-wide infra), and the data-flow walkthrough.

See [`../docs/crd-reference/`](../docs/crd-reference/) for the field-level CRD reference.

## Adding a CRD

```bash
# scaffold
go run sigs.k8s.io/kubebuilder/v4/cmd kubebuilder create api \
  --group agents --version v1alpha1 --kind <NewKind> --controller --resource

# regenerate: deepcopy, RBAC, CRD YAMLs, chart copy, Markdown reference
make manifests
```

Then write the controller body in `internal/controller/<newkind>_controller.go` (Reconcile + setup) plus `<newkind>_reconcile.go` (the actual work), add a conformance test in `test/conformance/`, and the kind will appear in the regenerated [`../docs/crd-reference/v1alpha1.md`](../docs/crd-reference/v1alpha1.md) automatically.
