variable "cluster_name" {
  description = "EKS cluster name — used to namespace SSM parameters and tags"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars. This component only uses it for SSM paths and tags, but the same token prefixes cluster-scoped S3 bucket names in landing-zone's agent-iam (<cluster>-<account>-<region>-<purpose>), which must stay inside S3's 63-char limit. Caught here because a cluster name is chosen once and reused across repos, and the bucket that would reject it is created several components later."
  }
}

variable "enable_guardrails_baseline" {
  description = "Create a baseline Bedrock Guardrail that the operator can reference. Tenant-specific guardrails attach per route via ModelGateway guardrailRef at reconcile time."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Common tags applied to all resources"
  type        = map(string)
  default     = {}
}
