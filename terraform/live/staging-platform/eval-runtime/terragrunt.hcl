include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/eval-runtime"
}

locals {
  env        = read_terragrunt_config(find_in_parent_folders("env.hcl"))
  account_id = local.env.locals.account_id
}

inputs = {
  # Staging starts to look like prod — pin to inference profiles when the
  # suites you exercise are stable.
  # Pinned to cross-region inference-profile ARNs rather than "*". A wildcard
  # here is a path around the per-Platform model scoping the operator generates:
  # an eval-runner pod holding it invokes every model in the account whatever a
  # Platform's allowedModels says. The environment does not change that — a
  # development account is still an account.
  #
  # Inference-profile ARNs, not foundation-model ARNs, so a model deprecation in
  # one region does not break eval runs in the other. account_id is interpolated
  # from env.hcl rather than duplicated.
  bedrock_invoke_resource_arns = [
    "arn:aws:bedrock:us-west-2:${local.account_id}:inference-profile/us.anthropic.claude-sonnet-5",
    "arn:aws:bedrock:us-east-1:${local.account_id}:inference-profile/us.anthropic.claude-sonnet-5",
  ]
  allowed_regions              = ["us-west-2", "us-east-1"]


  log_retention_days = 90
}
