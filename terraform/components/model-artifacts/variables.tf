variable "environment" {
  description = "Environment name"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped S3 bucket names (e.g. <cluster_name>-artifacts-eval-reports-<account>) that must stay within S3's 63-char limit."
  }
}

variable "data_kms_key_arn" {
  description = "KMS key ARN for encrypting model artifacts (cmk-data from landing-zone)"
  type        = string
}

variable "lifecycle_noncurrent_expiration_days" {
  description = "Delete non-current versions after N days"
  type        = number
  default     = 90
}

variable "access_logs_retention_days" {
  description = "Retention for S3 server-access logs in the access-logs bucket"
  type        = number
  default     = 365
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
