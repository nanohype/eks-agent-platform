# staging environment — replace REPLACE_* placeholders before apply
include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/accelerator-pools"
}

inputs = {
  oidc_provider_arn      = "arn:aws:iam::REPLACE:oidc-provider/oidc.eks.us-west-2.amazonaws.com/id/REPLACE"
  oidc_issuer            = "https://oidc.eks.us-west-2.amazonaws.com/id/REPLACE"
  node_role_name         = "eks-dev-karpenter-node"
  neuron_addon_namespace = "kube-system"
  gpu_operator_namespace = "gpu-operator"
}
