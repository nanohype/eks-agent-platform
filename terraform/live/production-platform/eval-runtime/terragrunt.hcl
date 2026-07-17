include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/eval-runtime"
}

locals {
  env        = read_terragrunt_config(find_in_parent_folders("env.hcl"))
  account_id = local.env.locals.account_id
}

# Required inputs sourced from the orchestrator (portal workspace
# variables for the production deploy):
#   - data_kms_key_arn, logs_kms_key_arn   (from lz-secrets)
inputs = {
  # Pin to specific cross-region inference profile ARNs the suites
  # actually exercise. Use bedrock:InferenceProfile ARNs (not foundation-
  # model ARNs) so a model deprecation in a region doesn't break eval
  # runs in the other. account_id is interpolated from env.hcl rather
  # than duplicated.
  bedrock_invoke_resource_arns = [
    "arn:aws:bedrock:us-west-2:${local.account_id}:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0",
    "arn:aws:bedrock:us-east-1:${local.account_id}:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0",
  ]
  allowed_regions = ["us-west-2", "us-east-1"]

  log_retention_days = 365
}
