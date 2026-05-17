# gitops/

ArgoCD overlay that adds the agent-platform layer on top of [`eks-gitops`](https://github.com/stxkxs/eks-gitops). Everything here installs into clusters labeled `eks-agent-platform/enabled=true`.

## What it ships

| Addon                      | Source                                                                                  | Sync wave |
| -------------------------- | --------------------------------------------------------------------------------------- | --------- |
| `gpu-operator`             | Helm — `nvidia/gpu-operator@v26.3.1`                                                    | 10        |
| `aws-neuron-device-plugin` | Kustomize — DaemonSet + ServiceAccount + ClusterRoleBinding                             | 10        |
| `nvidia-dra-driver`        | Helm — `oci://nvcr.io/nvidia/k8s/k8s-dra-driver-gpu@25.8.0`                             | 11        |
| `agentgateway`             | Helm — `oci://ghcr.io/agentgateway/agentgateway/charts/agentgateway@1.0.1`              | 20        |
| `operator`                 | Helm — `oci://ghcr.io/stxkxs/eks-agent-platform/charts/operator@0.1.0` (from this repo) | 21        |
| `kagent`                   | Helm — `oci://ghcr.io/kagent-dev/kagent/helm/kagent@0.9.4`                              | 30        |

Sync-wave ordering: device plugins first (cluster-level), then the data plane (agentgateway + operator + CRDs), then the agent runtime (kagent).

## How to attach to eks-gitops

This repo is a _delegated source_ for `eks-gitops`. In your `eks-gitops` repo:

1. Add this repo as a `sourceRepo` in the `platform` `AppProject`, or use the `eks-agent-platform` `AppProject` (see `gitops/applicationsets/app-project.yaml`).
2. Add an ApplicationSet that points at this repo's `gitops/applicationsets/` so Argo picks up new addons automatically:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: eks-agent-platform-bridge
  namespace: argocd
spec:
  generators:
    - git:
        repoURL: https://github.com/stxkxs/eks-agent-platform.git
        revision: main
        directories:
          - path: gitops/applicationsets/*
  template:
    spec:
      project: eks-agent-platform
      source:
        repoURL: https://github.com/stxkxs/eks-agent-platform.git
        targetRevision: main
        path: '{{path}}'
      destination:
        server: https://kubernetes.default.svc
        namespace: argocd
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
```

3. Label any clusters that should receive this overlay:

```bash
kubectl label secret <cluster-secret> -n argocd eks-agent-platform/enabled=true
```

The `gitops/environments/<env>/cluster-config.yaml` files show the full envelope including the env labels (`cluster_name`, `region`, `environment`) consumed by the matrix generators.

## Layout

```
gitops/
├── applicationsets/
│   ├── app-project.yaml           # AppProject scoping
│   ├── agent-platform.yaml        # Helm-driven addons
│   └── kustomize-only.yaml        # Pure-YAML addons (Neuron device plugin)
├── addons/
│   ├── gpu-operator/              # base + per-env values
│   ├── aws-neuron-device-plugin/  # kustomization + manifests
│   ├── dra-driver/                # base + per-env values
│   ├── agentgateway/              # base + per-env values
│   ├── eks-agent-platform-operator/  # base + per-env values
│   └── kagent/                    # base + per-env values
├── environments/
│   ├── dev/cluster-config.yaml
│   ├── staging/cluster-config.yaml
│   └── production/cluster-config.yaml
└── dashboards/                    # role-flexed Grafana JSON
    ├── finance.json
    ├── ops.json
    └── founder.json
```

## Required CRDs / capabilities in the host cluster

- `eks-gitops` already provides: cert-manager, external-secrets, ALB Controller, Cilium, OTel Collector, Kyverno, Prometheus operator CRDs, Karpenter, Argo Rollouts + Workflows.
- `eks-agent-platform` additionally requires DRA support (Kubernetes 1.32+ with the `DynamicResourceAllocation` feature gate). On EKS it's on by default in 1.32+.

## Adding a new addon

See [`CONTRIBUTING.md`](../CONTRIBUTING.md#adding-a-gitops-addon).
