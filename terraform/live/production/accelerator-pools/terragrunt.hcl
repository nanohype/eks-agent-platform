include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/accelerator-pools"
}

# Required inputs sourced from the orchestrator (tofui workspace
# variables for the production deploy):
#   - oidc_provider_arn, oidc_issuer  (from lz-cluster)
#   - node_role_name                  (from lz-cluster.karpenter_node_role_name;
#                                      timestamped, changes on cluster recreate)
inputs = {
  neuron_addon_namespace = "kube-system"
  gpu_operator_namespace = "gpu-operator"
}
