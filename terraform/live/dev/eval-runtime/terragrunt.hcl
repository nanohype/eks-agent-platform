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
  eval_reports_bucket_arn  = dependency.model_artifacts.outputs.eval_reports_bucket_arn
  eval_reports_bucket_name = dependency.model_artifacts.outputs.eval_reports_bucket_name

  # Dev: '*' Bedrock invoke is fine. Production pins to inference profiles.
  bedrock_invoke_resource_arns = ["*"]
  allowed_regions              = ["us-west-2", "us-east-1"]


  log_retention_days = 30
}
