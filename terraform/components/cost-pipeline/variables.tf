variable "environment" {
  description = "Environment name"
  type        = string
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped S3 bucket names (e.g. <cluster_name>-cost-access-logs-<account>) that must stay within S3's 63-char limit."
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

variable "bedrock_invocation_log_group" {
  description = "Bedrock invocation log group name (from terraform/components/bedrock outputs). The invocation-cost-publisher Lambda subscribes here."
  type        = string
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

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
