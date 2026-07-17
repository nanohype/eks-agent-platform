include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/kill-switch"
}

# All inputs are sourced from the orchestrator (portal workspace variables for
# the production deploy): logs_kms_key_arn (from lz-secrets) as TF_VAR; the
# operator role / tenant IAM path / tenant baseline policy from landing-zone's
# agent-iam SSM contract, read in-component.
inputs = {}
