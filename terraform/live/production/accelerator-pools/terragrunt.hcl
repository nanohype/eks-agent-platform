include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/accelerator-pools"
}

# Required inputs sourced from the orchestrator (portal workspace
# variables for the production deploy):
#   - node_role_name  (from lz-cluster.karpenter_node_role_name;
#                      timestamped, changes on cluster recreate)
inputs = {
  neuron_addon_namespace = "aws-neuron"
  gpu_operator_namespace = "gpu-operator"
}
