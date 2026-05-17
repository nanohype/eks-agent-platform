# production environment — replace REPLACE_* placeholders before apply
include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/bedrock"
}

inputs = {
  # Wire to landing-zone outputs (SSM Parameter or remote state).
  # Replace with `dependency "landing_zone_secrets"` block once landing-zone
  # publishes a stable output contract for this environment.
  logs_kms_key_arn = "arn:aws:kms:us-west-2:REPLACE:key/REPLACE-cmk-logs"

  log_retention_days = 365
  # COMPLIANCE mode in production: even root cannot shorten the retention
  # once an object is locked. GOVERNANCE allows a bypass with the
  # s3:BypassGovernanceRetention permission; not appropriate for production
  # audit trails where regulators expect cryptographic immutability.
  object_lock_mode           = "COMPLIANCE"
  object_lock_retention_days = 2555 # 7 years — typical SOC2/HIPAA audit retention floor
  enable_guardrails_baseline = true
}
