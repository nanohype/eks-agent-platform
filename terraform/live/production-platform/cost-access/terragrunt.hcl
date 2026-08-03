include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/cost-access"
}

# This cluster's access to the ACCOUNT's cost pipeline (live/org/cost-pipeline),
# which is applied once because a Cost and Usage Report always covers the whole
# account. What is per-cluster is the operator's IAM grant and the SSM handles the
# operator sweeps — both of which are this leaf.
#
# No `dependency` on the account root. It resolves at terragrunt PARSE time, so a
# missing account state would fail `init` here rather than `apply`, and no TF_VAR
# gets you out of that. The account's handles arrive over SSM instead; apply
# live/org/cost-pipeline first.
#
# This root takes no orchestrator TF_VARs. Everything it needs — the account's ARNs,
# the IAM path it checks against, and the KMS key the operator's grant is scoped to —
# arrives from the account contract that cost-pipeline publishes, so there is no value
# here that could disagree with the pipeline it grants access to.
inputs = {}
