# staging environment — replace REPLACE_* placeholders before apply
include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/kill-switch"
}

dependency "agent_iam" {
  config_path = "../agent-iam"

  mock_outputs = {
    operator_role_arn          = "arn:aws:iam::000000000000:role/mock-operator"
    tenant_iam_path            = "/eks-agent-platform/tenants/"
    tenant_baseline_policy_arn = "arn:aws:iam::000000000000:policy/mock-baseline"
  }
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

inputs = {
  tenant_iam_path            = dependency.agent_iam.outputs.tenant_iam_path
  tenant_baseline_policy_arn = dependency.agent_iam.outputs.tenant_baseline_policy_arn
  operator_role_arn          = dependency.agent_iam.outputs.operator_role_arn
  logs_kms_key_arn           = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-logs"
}
