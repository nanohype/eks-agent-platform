include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/bedrock"
}

# The invocation-logging half moved to live/org/bedrock-account — one configuration
# per account, not one per environment. This leaf now owns only the guardrail, and
# reads the account's log group and bucket over SSM to republish under its own prefix.
inputs = {
  enable_guardrails_baseline = true
}
