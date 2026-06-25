include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/accelerator-pools"
}

inputs = {
  node_role_name         = "eks-dev-karpenter-node"
  neuron_addon_namespace = "aws-neuron"
  gpu_operator_namespace = "gpu-operator"
}
