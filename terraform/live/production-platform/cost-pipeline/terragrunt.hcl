include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/cost-pipeline"
}

dependency "bedrock" {
  config_path = "../bedrock"

  mock_outputs = {
    invocation_log_group_name = "mock-bedrock-invocations"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

# Required inputs sourced from the orchestrator (portal workspace
# variables for the production deploy):
#   - data_kms_key_arn, logs_kms_key_arn  (from lz-secrets)
inputs = {
  cur_report_name               = "eks-agent-platform-production"
  bedrock_invocation_log_group  = dependency.bedrock.outputs.invocation_log_group_name
  athena_results_retention_days = 365 # production audit cycle; raise to 2555 for 7-year regulator floor
}
