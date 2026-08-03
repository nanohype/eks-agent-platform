variable "environment" {
  description = "Scope token for this root. Always `org` — a Cost and Usage Report has no filter and always covers the whole account, so this pipeline is applied once and never per workload environment. Carried for tagging only; nothing branches on it, because there is no environment whose teardown posture could apply."
  type        = string
  default     = "org"

  validation {
    condition     = var.environment == "org"
    error_message = "cost-pipeline is account-scoped: its only valid environment token is `org` (nanohype/standards/resource-naming.json). A workload environment token here would mean three roots each holding a complete duplicate copy of the same account-wide billing data — three reports, three buckets, three crawlers, three catalogs — which is the defect this component was reshaped to remove."
  }
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "tenant_iam_path" {
  description = <<-EOT
    IAM path every role the operator mints lives under — tenant roles and attribution
    session roles alike. The cost publisher's tag-read grant is scoped to it.

    It is an input rather than a read of agent-iam's SSM contract because that contract is
    published per CLUSTER and this component is applied once for the account: there is no
    single cluster subtree to read. The value is an account-wide constant in landing-zone
    (agent-iam's local.tenant_role_path), which is what makes one input able to stand for
    every cluster.

    It is not an unchecked mirror. This component publishes the path it used, and every
    cluster's cost-access compares it against that cluster's own agent-iam parameter and
    refuses to mint the operator grant if they disagree — so drift is caught once per
    cluster, by the layer that can see both values.
  EOT
  type        = string
  default     = "/eks-agent-platform/tenants/"

  validation {
    # Absolute AND a real subtree. "/" is absolute, and it is the account root: the
    # publisher's grant renders as `:role/*`, which is iam:ListRoleTags over every role
    # in the account. That is broader than the `Resource = ["*"]` form the suite already
    # rejects, and it arrives through a value that looks like a path rather than a
    # wildcard — so the wildcard check never sees it and this is the only place it can
    # be stopped.
    condition     = startswith(var.tenant_iam_path, "/") && length(trimspace(var.tenant_iam_path)) > 1 && !strcontains(var.tenant_iam_path, "//")
    error_message = "tenant_iam_path must be an absolute IAM path naming a subtree, with no empty segments — \"/\" is the account root, and scoping the publisher's tag-read grant there grants iam:ListRoleTags on every role in the account."
  }
}

variable "data_kms_key_arn" {
  description = "cmk-data for CUR report bucket encryption"
  type        = string
}

variable "athena_results_retention_days" {
  description = "How long to retain saved query outputs in the Athena results bucket. Default 30 is fine for dev (throwaway queries); production should bump to match the audit cycle — set to 90 or 365 depending on regulator requirements."
  type        = number
  default     = 30
  validation {
    condition     = var.athena_results_retention_days >= 1 && var.athena_results_retention_days <= 3650
    error_message = "athena_results_retention_days must be between 1 and 3650 (10 years)."
  }
}

variable "cur_crawler_schedule" {
  description = "Cron expression for the CUR Glue Crawler. AWS publishes CUR partitions hourly with the rest of the previous hour catching up over a ~6h window; daily 06:00 UTC picks up yesterday's full day plus the prior-day backfills."
  type        = string
  default     = "cron(0 6 * * ? *)"
}

variable "logs_kms_key_arn" {
  description = "cmk-logs ARN — the invocation-cost-publisher Lambda's own log group is encrypted here."
  type        = string
}

variable "invocation_cost_publisher_log_retention_days" {
  description = "How long to retain the invocation-cost-publisher Lambda's own CloudWatch logs"
  type        = number
  default     = 30
}

variable "access_logs_retention_days" {
  description = "Retention for S3 server-access logs in the access-logs bucket"
  type        = number
  default     = 365
}

variable "estimate_retention_days" {
  description = "How long to retain per-batch invocation-cost estimate NDJSON objects under the CUR bucket's estimates/ prefix. The reconciliation view only needs recent days (estimate-vs-CUR drift), so the default bounds object accumulation without losing useful history."
  type        = number
  default     = 90
  validation {
    condition     = var.estimate_retention_days >= 1 && var.estimate_retention_days <= 3650
    error_message = "estimate_retention_days must be between 1 and 3650 (10 years)."
  }
}

variable "imported_model_estimate_usd_per_mtokens" {
  description = <<-EOT
    Per-token governance estimate (USD per 1,000,000 input+output tokens) applied
    to Bedrock Custom Model Import (open-weight) model invocations, which are
    capacity-billed (CMUs) and so have no derivable per-token rate. 0 (default)
    leaves imported invocations unpriced — still observable via the
    UnpricedInvocations metric, but not in the EstimatedInvocationCostUsd signal
    the kill-switch reads. Set a conservative (rather high) value on an account
    that serves imported models so their spend trips the kill-switch; CUR remains
    authoritative for the actual bill. Threshold knob, not finance-grade.
  EOT
  type        = number
  default     = 0
  validation {
    condition     = var.imported_model_estimate_usd_per_mtokens >= 0
    error_message = "imported_model_estimate_usd_per_mtokens must be non-negative."
  }
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}

variable "force_destroy_buckets" {
  description = <<-EOT
    Allow this component's S3 buckets to be destroyed while they still hold objects, in any
    environment. Development already allows it unconditionally; this is the opt-in for
    everywhere else.

    It exists because a cluster here is an agent-managed, often short-lived thing — eks-fleet
    vends spokes with a ttlDays and a hub reaper that deletes them on expiry — so a teardown is
    an ordinary lifecycle event rather than an emergency. Without this, a reverse teardown
    wedges on BucketNotEmpty and leaves the cluster, VPC and NAT gateways standing and billing,
    which is the outcome the teardown existed to prevent.

    Deliberately two acts, not one flag: force_destroy has no effect until a successful apply
    lands it in state, so an operator (or an agent) must apply with this set and only then
    destroy. There is no single command that reaches a populated production bucket.

    What it exposes: the CUR delivery — this environment's billing history, which AWS only
    re-delivers while a month is inside its refresh window — plus Athena query results and the
    access logs over both. The cost data is the input to every budget decision, so a teardown
    here loses the record of what was spent, not just the substrate that spent it.
  EOT
  type        = bool
  default     = false
}
