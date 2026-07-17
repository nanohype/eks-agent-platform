include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/model-artifacts"
}

inputs = {
  lifecycle_noncurrent_expiration_days = 90
}
