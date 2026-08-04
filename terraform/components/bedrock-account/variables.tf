variable "environment" {
  description = "Scope token for this root. Always `org` — this component owns account+region singletons, so it is applied once for the account and never per workload environment. The value is carried for tagging only; nothing here branches on it, because there is no environment whose teardown posture could apply."
  type        = string
  default     = "org"

  validation {
    condition     = var.environment == "org"
    error_message = "bedrock-account is account-scoped: its only valid environment token is `org` (nanohype/standards/resource-naming.json). A workload environment token here would mean three roots claiming one account-wide logging configuration, which is the defect this component exists to remove."
  }
}

variable "logs_kms_key_arn" {
  description = "KMS key ARN for encrypting Bedrock invocation logs — landing-zone's log-path key (its own CMK where separate_logs_key is set, otherwise the platform key)."
  type        = string
}

variable "log_retention_days" {
  description = "How long to retain Bedrock invocation logs in CloudWatch"
  type        = number
  default     = 365
}

variable "object_lock_mode" {
  description = <<-EOT
    Object Lock mode on the invocation logging bucket. This is a retention decision about the
    model invocation record, deliberately outside force_destroy_buckets — a teardown flag must
    not be able to talk over a compliance statement.

    GOVERNANCE (default): the retention holds, and a principal carrying
    `s3:BypassGovernanceRetention` can clear it so the account tears down. Neither repo grants
    that action by name, but AdministratorAccess implies it, and landing-zone attaches that to
    the break-glass role in every workload environment and to the Admin SSO permission set — so
    read GOVERNANCE as "an administrator can delete the record", not as a barrier. An operator
    running a teardown without it sees the bucket refuse, object by object, with the lock as the
    reason.

    COMPLIANCE: nothing and nobody can shorten the retention, including the root account. The
    bucket, and therefore this component, cannot be destroyed until every object's
    retain-until date has passed. Choose it when that is the point, not to be thorough.
  EOT
  type        = string
  default     = "GOVERNANCE"
  validation {
    condition     = contains(["GOVERNANCE", "COMPLIANCE"], var.object_lock_mode)
    error_message = "object_lock_mode must be GOVERNANCE or COMPLIANCE."
  }
}

variable "object_lock_retention_days" {
  description = "How long to lock objects in the invocation logging bucket"
  type        = number
  default     = 365
}

variable "access_logs_retention_days" {
  description = "Retention for S3 server-access logs in the access-logs bucket"
  type        = number
  default     = 365
}

variable "tags" {
  description = "Common tags applied to all resources"
  type        = map(string)
  default     = {}
}
