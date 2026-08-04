include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/cost-pipeline"
}

# Applied ONCE for the account. A Cost and Usage Report has no filter — it always
# covers the whole account — so per-environment reports are complete duplicates of one
# another rather than three views of anything.
#
# Depends on live/org/bedrock-account only in ordering, and that ordering is expressed
# through SSM rather than a terragrunt `dependency`: this root reads the account's
# invocation log-group name from the contract bedrock-account publishes. Apply
# bedrock-account first.
#
# Required inputs sourced from the orchestrator:
#   - data_kms_key_arn, logs_kms_key_arn  (from lz-secrets)
inputs = {
  # Query results for the whole account, including the reconciliation view's output.
  # Every cluster's budget decisions are audited against this one bucket, so the
  # retention is set to a full year rather than to the component's throwaway-query
  # default.
  athena_results_retention_days = 365
}
