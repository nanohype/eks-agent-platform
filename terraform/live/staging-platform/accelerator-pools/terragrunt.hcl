include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/accelerator-pools"
}

# node_role_name (the Karpenter node IAM role, which changes on cluster
# recreate) comes in as TF_VAR_node_role_name from the orchestrator, like
# production — it is not pinned here.
inputs = {
  neuron_addon_namespace = "aws-neuron"
  gpu_operator_namespace = "gpu-operator"
}
