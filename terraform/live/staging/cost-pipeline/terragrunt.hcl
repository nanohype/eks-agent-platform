# staging environment — replace REPLACE_* placeholders before apply
include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/cost-pipeline"
}

dependency "agent_iam" {
  config_path = "../agent-iam"

  mock_outputs = {
    operator_role_arn  = "arn:aws:iam::000000000000:role/mock-operator"
    operator_role_name = "mock-operator"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

dependency "bedrock" {
  config_path = "../bedrock"

  mock_outputs = {
    invocation_log_group_name = "mock-bedrock-invocations"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

inputs = {
  data_kms_key_arn = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-data"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-logs"

  operator_role_arn  = dependency.agent_iam.outputs.operator_role_arn
  operator_role_name = dependency.agent_iam.outputs.operator_role_name

  cur_report_name               = "eks-agent-platform-staging"
  bedrock_invocation_log_group  = dependency.bedrock.outputs.invocation_log_group_name
  athena_results_retention_days = 90 # spans a full monthly billing cycle + buffer for cost-audit replay
}
