include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/eval-runtime"
}

dependency "model_artifacts" {
  config_path = "../model-artifacts"

  mock_outputs = {
    eval_reports_bucket_arn  = "arn:aws:s3:::mock-eval-reports"
    eval_reports_bucket_name = "mock-eval-reports"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

inputs = {
  oidc_provider_arn = "arn:aws:iam::REPLACE:oidc-provider/oidc.eks.us-west-2.amazonaws.com/id/REPLACE"
  oidc_issuer       = "oidc.eks.us-west-2.amazonaws.com/id/REPLACE"

  eval_reports_bucket_arn  = dependency.model_artifacts.outputs.eval_reports_bucket_arn
  eval_reports_bucket_name = dependency.model_artifacts.outputs.eval_reports_bucket_name

  # Production: pin to specific cross-region inference profile ARNs the
  # suites actually exercise. Use bedrock:InferenceProfile ARNs rather than
  # foundation-model ARNs so a model deprecation in a region doesn't break
  # eval runs in the other.
  bedrock_invoke_resource_arns = [
    "arn:aws:bedrock:us-west-2:REPLACE:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0",
    "arn:aws:bedrock:us-east-1:REPLACE:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0",
  ]
  allowed_regions = ["us-west-2", "us-east-1"]

  data_kms_key_arn = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-data"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-logs"

  log_retention_days = 365
}
