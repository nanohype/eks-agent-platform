include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/model-artifacts"
}

inputs = {
  data_kms_key_arn                     = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-data"
  lifecycle_noncurrent_expiration_days = 90
}
