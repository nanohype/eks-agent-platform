# components/accelerator-pools

AWS-side prerequisites for accelerator scheduling. The actual Karpenter `NodePool` and DRA `DeviceClass` resources live in the eks-gitops `accelerators` category (they are Kubernetes resources, not AWS resources). This component provisions:

- **Pod Identity-bound roles** for the NVIDIA GPU Operator and the AWS Neuron device plugin (introspection-only permissions: `ec2:DescribeInstances`, `ec2:DescribeInstanceTypes`).
- **Karpenter node role extension** for Neuron topology discovery.
- **Pool catalog SSM parameter** — JSON document listing the five default accelerator pools (`nvidia-l4`, `nvidia-l40s`, `nvidia-h100`, `neuron-inf2`, `neuron-trn2`) with their instance types, capacity types, device class, and node labels. It is the source catalog for fleet-level accelerator scheduling; the operator-side consumption path is tracked in [#106](https://github.com/nanohype/eks-agent-platform/issues/106).

## Inputs

| Variable                                           | Description                                                |
| -------------------------------------------------- | ---------------------------------------------------------- |
| `environment`, `region`, `cluster_name`            | identifying                                                |
| `node_role_name`                                   | existing Karpenter node IAM role (managed in landing-zone) |
| `neuron_addon_namespace`, `gpu_operator_namespace` | defaults match the eks-gitops accelerator values           |

Each role is bound to its ServiceAccount by an EKS Pod Identity association — `neuron-device-plugin` in `aws-neuron` and `gpu-operator` in `gpu-operator`. No IRSA role-arn annotation is set on those ServiceAccounts.

## Outputs

- `neuron_role_arn`, `gpu_operator_role_arn` — the Pod Identity-bound roles for the Neuron device plugin and the NVIDIA GPU Operator
- `pool_catalog_ssm_path` — SSM path to the accelerator pool catalog
