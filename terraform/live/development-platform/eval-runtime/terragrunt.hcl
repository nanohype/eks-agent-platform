include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/eval-runtime"
}

inputs = {
  log_retention_days = 30
}
