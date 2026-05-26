locals {
  environment   = "production"
  region        = "us-west-2"
  cluster_name  = "production-eks"
  cost_center   = "engineering"
  business_unit = "platform"

  # account_id is the only environment-identifying value that has to live
  # in git: terragrunt's `remote_state.config.bucket` evaluates this at
  # parse time, so the backend bucket name needs it before any AWS API
  # is reachable. All other infrastructure identifiers (OIDC issuer,
  # VPC/subnet IDs, KMS key ARN, route tables, security group, Karpenter
  # node-role name) come in as `TF_VAR_*` from the orchestrator (tofui
  # workspace variables for the production deploy). Leaves declare the
  # variables in `variables.tf`; if a leaf is run outside tofui, set the
  # corresponding TF_VAR_ in the shell.
  account_id = "111111111111"
}
