# Integrating with eks-gitops

`eks-agent-platform/gitops/` is an _overlay_ on top of [`eks-gitops`](https://github.com/stxkxs/eks-gitops). It does not replace `eks-gitops` and does not stand alone.

## What eks-gitops provides

- ArgoCD (deployed by `aws-eks` CDK)
- cert-manager, external-secrets, ALB Controller, External DNS
- Cilium (CNI + NetworkPolicy enforcement)
- Kyverno (verify-images, PSS policies)
- Prometheus Operator CRDs, Loki, Tempo, Grafana Agent, OpenCost
- Velero, VPA, Goldilocks, Descheduler, Karpenter, KEDA
- Argo Rollouts, Events, Workflows

All of which are prerequisites for the agent platform.

## What this repo adds

- ApplicationSets for the agent-specific addons (kagent, agentgateway, GPU operator, Neuron device plugin, DRA driver, this repo's operator)
- Helm values + per-environment overrides for each
- ArgoCD `AppProject` scoped to those addons + any tenant namespaces
- Role-flexed Grafana dashboards

## Hook up

Once per cluster:

1. Apply `gitops/applicationsets/app-project.yaml` to create the `eks-agent-platform` AppProject in `argocd` namespace.
2. Apply the cluster-config secret in `gitops/environments/<env>/cluster-config.yaml` so the matrix generator's selector matches. This labels the in-cluster ArgoCD secret with `eks-agent-platform/enabled=true` plus env metadata.
3. Add an ApplicationSet to your `eks-gitops` repo that watches this repo's `gitops/applicationsets/` directory (template in `gitops/README.md`).

That's it. ArgoCD picks up the addons in the order set by sync waves (device plugins → gateway + operator → kagent), and any future ApplicationSet committed to this repo flows in automatically.

## Validating before applying

```bash
# In this repo
task gitops:validate     # kubectl dry-run on every ApplicationSet
task helm:lint           # lint every chart
task helm:template       # render every chart against defaults
```

CI runs the same.
