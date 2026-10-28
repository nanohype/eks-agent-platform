include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/eval-runtime"
}

# Required input sourced from the orchestrator (portal workspace variables for
# the production deploy): logs_kms_key_arn (from lz-secrets).
inputs = {
  log_retention_days = 365
}
