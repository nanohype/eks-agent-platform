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

dependency "model_artifacts" {
  config_path = "../model-artifacts"

  mock_outputs = {
    eval_reports_bucket_arn  = "arn:aws:s3:::mock-eval-reports"
    eval_reports_bucket_name = "mock-eval-reports"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

# Required inputs sourced from the orchestrator (portal workspace
# variables for the production deploy):
#   - oidc_provider_arn, oidc_issuer       (from lz-cluster)
#   - data_kms_key_arn, logs_kms_key_arn   (from lz-secrets)
inputs = {
  eval_reports_bucket_arn  = dependency.model_artifacts.outputs.eval_reports_bucket_arn
  eval_reports_bucket_name = dependency.model_artifacts.outputs.eval_reports_bucket_name

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
