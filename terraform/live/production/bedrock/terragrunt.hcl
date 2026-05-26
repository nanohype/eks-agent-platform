include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/bedrock"
}

# Required inputs sourced from the orchestrator (tofui workspace
# variables for the production deploy):
#   - logs_kms_key_arn  (from lz-secrets)
inputs = {
  log_retention_days = 365
  # COMPLIANCE mode in production: even root cannot shorten the retention
  # once an object is locked. GOVERNANCE allows a bypass with the
  # s3:BypassGovernanceRetention permission; not appropriate for production
  # audit trails where regulators expect cryptographic immutability.
  object_lock_mode           = "COMPLIANCE"
  object_lock_retention_days = 2555 # 7 years — typical SOC2/HIPAA audit retention floor
  enable_guardrails_baseline = true
}
