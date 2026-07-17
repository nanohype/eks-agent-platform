variable "environment" {
  description = "Environment name"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped IAM/SSM names; 27 is the tightest cluster-scoped budget (an S3 bucket in a sibling component) so every derived name stays within AWS limits."
  }
}

variable "logs_kms_key_arn" {
  description = "cmk-logs for Step Functions execution history encryption"
  type        = string
}

variable "tenant_role_name_pattern" {
  description = <<-EOT
    Step Functions builds the IRSA role name from this pattern at runtime:
    States.Format applied to the literal string with '{}' replaced by the
    BudgetBreach event's $.detail.platformId. The default is the contract
    the operator's PlatformReconciler MUST follow when minting per-tenant
    roles; changing the pattern here without updating the operator breaks
    the kill-switch silently (DetachRolePolicy against a nonexistent role
    routes to RecordFailure with no alarm).
  EOT
  type        = string
  default     = "<cluster>-{}-tenant"
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
