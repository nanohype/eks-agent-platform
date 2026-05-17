include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/agent-iam"
}

dependency "model_artifacts" {
  config_path = "../model-artifacts"

  mock_outputs = {
    artifacts_bucket_arn = "arn:aws:s3:::mock-artifacts-bucket"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

inputs = {
  oidc_provider_arn = "arn:aws:iam::REPLACE:oidc-provider/oidc.eks.us-west-2.amazonaws.com/id/REPLACE"
  oidc_issuer       = "https://oidc.eks.us-west-2.amazonaws.com/id/REPLACE"
  data_kms_key_arn  = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-data"

  artifacts_bucket_arn = dependency.model_artifacts.outputs.artifacts_bucket_arn

  operator_namespace       = "eks-agent-platform"
  operator_service_account = "operator"
  tenant_iam_path          = "/eks-agent-platform/tenants/"
}
