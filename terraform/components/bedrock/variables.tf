variable "environment" {
  description = "Environment name (dev, staging, production)"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name — used to namespace SSM parameters and tags"
  type        = string
}

variable "logs_kms_key_arn" {
  description = "KMS key ARN for encrypting Bedrock invocation logs (cmk-logs from landing-zone)"
  type        = string
}

variable "log_retention_days" {
  description = "How long to retain Bedrock invocation logs in CloudWatch"
  type        = number
  default     = 365
}

variable "object_lock_mode" {
  description = "Object Lock mode on the invocation logging bucket: GOVERNANCE or COMPLIANCE"
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

variable "enable_guardrails_baseline" {
  description = "Create a baseline Bedrock Guardrail that the operator can reference. Tenant-specific guardrails are managed by GuardrailPolicy CRs at reconcile time."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Common tags applied to all resources"
  type        = map(string)
  default     = {}
}
