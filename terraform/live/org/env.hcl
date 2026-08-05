locals {
  # Not a workload environment — the account itself. Two things in this tree exist
  # once per account and cannot be modelled per environment without three roots
  # fighting over one object:
  #
  #   the Bedrock invocation-logging configuration, of which AWS keeps exactly one
  #     per account per region, so the last apply silently repoints every
  #     environment's logs and any environment's destroy deletes it for all of them
  #   the Cost and Usage Report, which covers the whole
  #     account, so per-environment reports are complete duplicates of one another
  #
  # `org` is the reserved account-scope token from nanohype/standards/resource-naming.json,
  # and the same one landing-zone uses for its management-account roots. It occupies
  # the environment slot rather than adding a tier above it.
  #
  # There is deliberately no cluster_name. root.hcl reads it with try(), so a root
  # without a cluster passes nothing rather than a placeholder that would flow into
  # resource names as if it meant something.
  environment = "org"

  # The region follows the deployment, it is not pinned here. Both objects above
  # are account+REGION singletons, so a run against any other region configured
  # Bedrock invocation logging in us-west-2 — a region where nothing was
  # deployed — while the apply reported success. Nothing downstream disagreed,
  # because a logging configuration in the wrong region is a valid object.
  #
  # Read the same way account_id is, and for the same reason: it names the state
  # bucket in root.hcl before any AWS call, so it cannot arrive as a TF_VAR_.
  # Deliberately no default — a silent fallback to us-west-2 is the defect. The
  # orchestrator sets AWS_REGION on every invocation; for a manual run, export it.
  region = get_env("AWS_REGION")

  # account_id resolves at parse time from AWS_ACCOUNT_ID — it names the state
  # bucket in root.hcl before any AWS call, so it can't arrive as a TF_VAR_. The
  # orchestrator sets it; for a manual run, export AWS_ACCOUNT_ID in the shell.
  account_id    = get_env("AWS_ACCOUNT_ID")
  cost_center   = "engineering"
  business_unit = "platform"
}
