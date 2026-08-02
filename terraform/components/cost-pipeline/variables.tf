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
    condition     = startswith(var.tenant_iam_path, "/")
    error_message = "tenant_iam_path must be an absolute IAM path beginning with /."
  }
}

variable "data_kms_key_arn" {
  description = "cmk-data for CUR report bucket encryption"
  type        = string
}

variable "cur_report_name" {
  description = "Name of the Cost & Usage Report. Must be unique across the account."
  type        = string
  default     = "eks-agent-platform"
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

variable "cur_retention_days" {
  description = <<-EOT
    How long the CUR Parquet AWS delivers under `cur/` is kept. This is the billing history
    every budget decision reads back over, and AWS re-delivers the current month but never a
    closed one, so shortening it discards a record that cannot be recovered.

    Measured from each object's last delivery rather than from the billing month:
    report_versioning is OVERWRITE_REPORT and refresh_closed_reports is on, so AWS rewrites a
    month's objects while it remains inside its refresh window and the clock restarts. Past
    that window the objects are final, and this is what bounds them.

    It also bounds the bucket. Without an expiry here the CUR prefix grows for the life of the
    account, which is both an unbounded storage bill and a teardown with no end.
  EOT
  type        = number
  default     = 730

  validation {
    condition     = var.cur_retention_days >= 90 && var.cur_retention_days <= 3650
    error_message = "cur_retention_days must be between 90 (a quarter of billing history) and 3650 (10 years)."
  }

  validation {
    condition     = var.cur_retention_days >= var.estimate_retention_days
    error_message = "cur_retention_days must be >= estimate_retention_days — the reconciliation view joins the in-flight estimates against the CUR line items, so a CUR window shorter than the estimate window silently reconciles estimates against nothing."
  }
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
