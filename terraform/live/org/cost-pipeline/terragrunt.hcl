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
  # One report for the account. It carries no environment token because it has no
  # environment: the data is every environment's, and always was.
  cur_report_name = "eks-agent-platform"

  # The account's billing history is what every environment's budget reads, so the
  # retention is the longest of what the environments used to ask for individually
  # rather than the shortest.
  athena_results_retention_days = 365
}
