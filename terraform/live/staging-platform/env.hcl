locals {
  environment  = "staging"
  region       = "us-west-2"
  cluster_name = "staging-platform"
  # account_id resolves at parse time from AWS_ACCOUNT_ID — it names the state
  # bucket in root.hcl before any AWS call, so it can't arrive as a TF_VAR_. The
  # orchestrator sets it; for a manual run, export AWS_ACCOUNT_ID in the shell.
  # All other infrastructure identifiers (KMS key ARNs, VPC/subnet IDs, route
  # tables, security group, Karpenter node-role name) come in as TF_VAR_* from
  # the orchestrator, the same as production — export them too for a manual run.
  account_id    = get_env("AWS_ACCOUNT_ID")
  cost_center   = "engineering"
  business_unit = "platform"
}
