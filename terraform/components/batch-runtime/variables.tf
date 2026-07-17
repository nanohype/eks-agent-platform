variable "environment" {
  description = "Environment name (dev, staging, production)"
  type        = string
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name — used to namespace the role and SSM parameters"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped IAM/SSM names; 27 is the tightest cluster-scoped budget (an S3 bucket in a sibling component) so every derived name stays within AWS limits."
  }
}

variable "artifacts_bucket_arn" {
  description = "Model-artifacts S3 bucket ARN (from the model-artifacts component). Batch input/output JSONL is staged under its batch/ prefix; the Bedrock batch service role is scoped to that prefix."
  type        = string
}

variable "data_kms_key_arn" {
  description = "cmk-data ARN — the artifacts bucket is SSE-KMS-encrypted, so the batch service role needs scoped decrypt/generate via s3."
  type        = string
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
