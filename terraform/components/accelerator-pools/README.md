# components/accelerator-pools

AWS-side prerequisites for accelerator scheduling. The actual Karpenter `NodePool` and DRA `DeviceClass` resources live in `gitops/addons/` (they are Kubernetes resources, not AWS resources). This component provisions:

- **IRSA roles** for the NVIDIA GPU Operator and the AWS Neuron device plugin (introspection-only permissions: `ec2:DescribeInstances`, `ec2:DescribeInstanceTypes`).
- **Karpenter node role extension** for Neuron topology discovery.
- **Pool catalog SSM parameter** — JSON document listing the five default accelerator pools (`nvidia-l4`, `nvidia-l40s`, `nvidia-h100`, `neuron-inf2`, `neuron-trn2`) with their instance types, capacity types, device class, and node labels. The operator reads this when reconciling `AcceleratorPool` CRs so the controller doesn't hardcode an instance catalog.

## Inputs

| Variable                                           | Description                                                |
| -------------------------------------------------- | ---------------------------------------------------------- |
| `environment`, `region`, `cluster_name`            | identifying                                                |
| `oidc_provider_arn`, `oidc_issuer`                 | from landing-zone cluster outputs                          |
| `node_role_name`                                   | existing Karpenter node IAM role (managed in landing-zone) |
| `neuron_addon_namespace`, `gpu_operator_namespace` | defaults match the gitops chart values                     |

## Outputs

- `neuron_role_arn` — set as the IRSA annotation on the Neuron device plugin ServiceAccount in `gitops/addons/aws-neuron-device-plugin/values.yaml`
- `gpu_operator_role_arn` — same for `gitops/addons/gpu-operator/values.yaml`
- `pool_catalog_ssm_path` — operator config
