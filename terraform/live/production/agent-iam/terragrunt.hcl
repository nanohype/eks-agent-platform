include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/agent-iam"
}

dependency "model_artifacts" {
  config_path = "../model-artifacts"

  mock_outputs = {
    artifacts_bucket_arn = "arn:aws:s3:::mock-artifacts-bucket"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

# Required inputs sourced from the orchestrator (tofui workspace
# variables for the production deploy):
#   - oidc_provider_arn, oidc_issuer  (from lz-cluster)
#   - data_kms_key_arn                (from lz-secrets)
inputs = {
  artifacts_bucket_arn = dependency.model_artifacts.outputs.artifacts_bucket_arn

  operator_namespace       = "eks-agent-platform"
  operator_service_account = "operator"
  tenant_iam_path          = "/eks-agent-platform/tenants/"
}
