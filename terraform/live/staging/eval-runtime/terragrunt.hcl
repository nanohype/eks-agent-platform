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

  # Staging starts to look like prod — pin to inference profiles when the
  # suites you exercise are stable.
  bedrock_invoke_resource_arns = ["*"]
  allowed_regions              = ["us-west-2", "us-east-1"]

  data_kms_key_arn = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-data"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-logs"

  log_retention_days = 90
}
