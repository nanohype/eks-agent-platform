include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/accelerator-pools"
}

# Nothing is sourced from the orchestrator. This component no longer touches the
# Karpenter node role, so it takes no input that changes when the cluster is
# recreated.
inputs = {
  gpu_operator_namespace = "gpu-operator"
}
