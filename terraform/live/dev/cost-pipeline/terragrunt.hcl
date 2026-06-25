include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/cost-pipeline"
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

  cur_report_name              = "eks-agent-platform-dev"
  bedrock_invocation_log_group = dependency.bedrock.outputs.invocation_log_group_name
}
