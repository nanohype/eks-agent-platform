include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/eval-runtime"
}

inputs = {
  # Dev: '*' Bedrock invoke is fine. Production pins to inference profiles.
  bedrock_invoke_resource_arns = ["*"]
  allowed_regions              = ["us-west-2", "us-east-1"]


  log_retention_days = 30
}
