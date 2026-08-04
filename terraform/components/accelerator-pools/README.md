# components/accelerator-pools

The AWS half of accelerator scheduling, and nothing more. The Karpenter
`NodePool` and the DRA `DeviceClass` are Kubernetes resources and live in the
eks-gitops `accelerators` category; the nodes are Karpenter's, and it launches
one only when a pod requests `nvidia.com/gpu`.

So applying this costs nothing. It grants an identity; it does not reserve
capacity.

This component provisions one thing:

- **A Pod Identity-bound role for the NVIDIA GPU Operator**, with
  introspection-only permissions (`ec2:DescribeInstances`,
  `ec2:DescribeInstanceTypes`), bound to the `gpu-operator` ServiceAccount in the
  namespace the eks-gitops `addons-accelerators-helm` ApplicationSet installs
  into. No IRSA role-arn annotation is set on that ServiceAccount.

## No Neuron

There is no Inferentia or Trainium counterpart here, deliberately. eks-gitops
installs the GPU Operator and the NVIDIA DRA driver; it has no Neuron device
plugin addon. A role bound to a `neuron-device-plugin` ServiceAccount would be
bound to a ServiceAccount nothing creates — an identity for a workload that is
not deployed, which reads to anyone auditing IAM as though Neuron were
supported.

Neuron comes back as an eks-gitops addon first. This component follows it.

## No published outputs

This component publishes no SSM parameters and exports no outputs.

It used to publish three: the two role ARNs and a JSON catalog of accelerator
pool defaults. Nothing read any of them. The role ARNs are the IRSA paste seam
that EKS Pod Identity exists to remove — the binding is made here, by
`aws_eks_pod_identity_association`, so no consumer needs the ARN. The catalog
was a static instance-type table published for an `AcceleratorPool` CR that was
never built; the consumption path is tracked in
[#106](https://github.com/nanohype/eks-agent-platform/issues/106), and the
catalog belongs with the controller that reads it rather than ahead of it.

## Inputs

| Variable                 | Description                                                         |
| ------------------------ | ------------------------------------------------------------------- |
| `cluster_name`           | identifying; prefixes the role name                                 |
| `gpu_operator_namespace` | defaults to `gpu-operator`, matching the eks-gitops ApplicationSet   |
| `tags`                   | common tags                                                          |

`node_role_name` is gone. The only thing this component ever put on the Karpenter
node role was an `ec2:Describe*` policy for Neuron topology discovery; the GPU
Operator's node-side needs are covered by the AWS EKS-managed node role baseline.
