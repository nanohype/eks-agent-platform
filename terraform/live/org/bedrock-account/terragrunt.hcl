include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/bedrock-account"
}

# Applied ONCE for the account. AWS keeps exactly one Bedrock invocation-logging
# configuration per account per region, so there is no per-environment leaf of this —
# three of them would each silently repoint the account's logging at their own bucket.
#
# No `dependency` block, deliberately. This root is the first thing in the tree that
# has to exist: every per-environment bedrock leaf reads its published contract from
# SSM, and a terragrunt dependency pointing the other way would make those leaves fail
# `init` rather than `apply`.
#
# Required inputs sourced from the orchestrator:
#   - logs_kms_key_arn  (from lz-secrets)
inputs = {
  log_retention_days = 365
  # GOVERNANCE keeps invocation logs immutable by default while letting an admin
  # (s3:BypassGovernanceRetention) clear the lock, so the account tears down cleanly.
  # COMPLIANCE means nobody — including root — can delete the objects or the bucket
  # until retention expires. It now covers every environment's invocations, so choose
  # it because immutability is the point, not to be thorough.
  object_lock_mode           = "GOVERNANCE"
  object_lock_retention_days = 365
}
