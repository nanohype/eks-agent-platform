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

  log_retention_days         = 365
  object_lock_mode           = "GOVERNANCE"
  object_lock_retention_days = 365
  enable_guardrails_baseline = true
}
