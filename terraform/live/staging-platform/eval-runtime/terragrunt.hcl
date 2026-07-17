include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/eval-runtime"
}

inputs = {
  # Staging starts to look like prod — pin to inference profiles when the
  # suites you exercise are stable.
  bedrock_invoke_resource_arns = ["*"]
  allowed_regions              = ["us-west-2", "us-east-1"]


  log_retention_days = 90
}
