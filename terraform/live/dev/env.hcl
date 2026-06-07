locals {
  environment  = "dev"
  region       = "us-west-2"
  cluster_name = "eks-dev"
  # account_id resolves at parse time from AWS_ACCOUNT_ID — it names the state
  # bucket in root.hcl before any AWS call, so it can't arrive as a TF_VAR_. The
  # orchestrator sets it; for a manual run, export AWS_ACCOUNT_ID in the shell.
  account_id    = get_env("AWS_ACCOUNT_ID")
  cost_center   = "engineering"
  business_unit = "platform"
}
