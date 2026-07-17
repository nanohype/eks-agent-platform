locals {
  environment   = "production"
  region        = "us-west-2"
  cluster_name  = "production-platform"
  cost_center   = "engineering"
  business_unit = "platform"

  # account_id resolves at parse time from the AWS_ACCOUNT_ID environment
  # variable: terragrunt's `remote_state.config.bucket` embeds it and is
  # evaluated before any AWS API is reachable, so it can't arrive as a
  # `TF_VAR_` (those reach the leaf module, not the backend config) — and it
  # stays out of git. All other infrastructure identifiers (KMS key ARNs,
  # VPC/subnet IDs, route tables, security group, Karpenter node-role name)
  # come in as `TF_VAR_*` from the orchestrator (portal
  # workspace variables for the production deploy). Leaves declare the
  # variables in `variables.tf`. The orchestrator sets AWS_ACCOUNT_ID; if a
  # leaf is run outside portal, export AWS_ACCOUNT_ID (and the TF_VAR_s) first.
  account_id = get_env("AWS_ACCOUNT_ID")
}
