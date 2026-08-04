variable "environment" {
  description = "Scope token for this root. Always `org` — a Cost and Usage Report has no filter and always covers the whole account, so this pipeline is applied once and never per workload environment. Carried for tagging only; nothing branches on it, because there is no environment whose teardown posture could apply."
  type        = string
  default     = "org"

  validation {
    condition     = var.environment == "org"
    error_message = "cost-pipeline is account-scoped: its only valid environment token is `org` (nanohype/standards/resource-naming.json). A workload environment token here would mean one root per environment, each holding a complete duplicate copy of the same account-wide billing data — N buckets, N catalogs, N publishers all processing every record into the same account-global metric namespace, and every copy correct."
  }
}

# There is deliberately no `region` variable. Everything this component names, places
# or grants on is regional, and the region that decides all of it is the one the
# provider is configured with — read once from data.aws_region.current. A variable
# beside it would be a second answer to a question with one authority, and the two
# disagreeing is not an error anything reports: the buckets, the workgroup and the
# subscription filter land in the provider's region while the publisher's KMS condition
# and the log-group grant name the variable's, and both halves apply green.

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
  description = <<-EOT
    The account's cost CMK. One key governs four things at once, which is why it is a
    single input rather than one per site: the Athena workgroup's ENFORCED result
    encryption, the results bucket's SSE default, the estimates bucket's SSE default,
    and the SSE-KMS header the cost publisher stamps on every estimate object.

    It is also published to SSM, because each cluster's cost-access scopes its operator's
    KMS grant to it and has no way to check that value against anything — the account is
    the only authority on which key encrypts cost data. A grant on a key nothing uses
    fails every query at the SSE-KMS WRITE step, which the reconciler records as
    unreadable spend rather than as access denied.
  EOT
  type        = string
}

variable "athena_results_retention_days" {
  description = "How long to retain saved query outputs in the Athena results bucket. These are the account's own query results — the reconciliation view's output and whatever an analyst ran — not billing history, which lives in landing-zone's export. Set it to whatever the audit cycle asks for."
  type        = number
  default     = 30
  validation {
    condition     = var.athena_results_retention_days >= 1 && var.athena_results_retention_days <= 3650
    error_message = "athena_results_retention_days must be between 1 and 3650 (10 years)."
  }
}

# There is no crawler schedule here. The CUR table is declared with its partitions
# projected, so nothing has to run for a billing period to become queryable — see
# the table block in main.tf for why a scheduled crawl is the wrong shape for a
# kill switch that reads month-to-date spend.

variable "logs_kms_key_arn" {
  description = "Log-path KMS key ARN — the invocation-cost-publisher Lambda's own log group is encrypted here. Landing-zone's platform CMK unless that environment sets separate_logs_key."
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
  description = "How long to retain per-batch invocation-cost estimate NDJSON objects under the estimates bucket's estimates/ prefix. The reconciliation view only needs recent days (estimate-vs-CUR drift), so the default bounds object accumulation without losing useful history."
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
    Allow this component's S3 buckets to be destroyed while they still hold objects. There
    is no environment to branch on — this root is applied once for the account — so the
    lever is this flag and nothing else.

    It exists because a cluster here is an agent-managed, often short-lived thing — eks-fleet
    vends spokes with a ttlDays and a hub reaper that deletes them on expiry — so a teardown is
    an ordinary lifecycle event rather than an emergency. Without this, a reverse teardown
    wedges on BucketNotEmpty and leaves the cluster, VPC and NAT gateways standing and billing,
    which is the outcome the teardown existed to prevent.

    Deliberately two acts, not one flag: force_destroy has no effect until a successful apply
    lands it in state, so an operator (or an agent) must apply with this set and only then
    destroy. There is no single command that reaches a populated production bucket.

    What it exposes: the invocation-cost estimates, which are this account's only
    sub-CUR-partition record of what each tenant spent. They are re-derivable only while the
    Bedrock invocation logs behind them are still inside their own retention, so past that
    window a teardown loses them for good. Plus Athena query results and the access logs over
    both. The account's billing detail itself is safe: it lives in landing-zone's export
    bucket, which this component only reads.
  EOT
  type        = bool
  default     = false
}
