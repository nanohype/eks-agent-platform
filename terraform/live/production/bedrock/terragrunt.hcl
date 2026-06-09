include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/bedrock"
}

# Required inputs sourced from the orchestrator (portal workspace
# variables for the production deploy):
#   - logs_kms_key_arn  (from lz-secrets)
inputs = {
  log_retention_days = 365
  # GOVERNANCE keeps invocation logs immutable by default while letting an
  # admin (s3:BypassGovernanceRetention) clear the lock, so the environment
  # tears down cleanly. Switch to COMPLIANCE for a regulated tenant that needs
  # cryptographic immutability — note that COMPLIANCE-locked objects, and the
  # bucket itself, then cannot be deleted by anyone (including root) until the
  # retention period expires.
  object_lock_mode           = "GOVERNANCE"
  object_lock_retention_days = 365
  enable_guardrails_baseline = true
}
