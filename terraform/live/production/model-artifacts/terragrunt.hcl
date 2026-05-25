include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/model-artifacts"
}

# Required inputs sourced from the orchestrator (tofui workspace
# variables for the production deploy):
#   - data_kms_key_arn  (from lz-secrets)
inputs = {
  lifecycle_noncurrent_expiration_days = 90
}
