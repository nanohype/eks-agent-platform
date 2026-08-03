variable "environment" {
  description = "Workload environment this cluster serves. Carried for tagging; this component creates no storage, so it has no teardown posture to branch on."
  type        = string

  validation {
    condition     = contains(["development", "staging", "production"], var.environment)
    error_message = "environment must be development, staging or production. The account-scoped cost pipeline uses `org`; this component is its per-cluster counterpart and always belongs to a workload environment."
  }
}

variable "region" {
  description = "AWS region. Used to compose the Glue catalog ARN and to bind the operator's KMS grant to S3 in this region."
  type        = string
}

variable "cluster_name" {
  description = "Full EKS cluster name (<environment>-<clusterBase>). Keys both the agent-iam contract this component reads and the SSM subtree it republishes into — the same subtree the operator sweeps, so co-located sibling clusters resolve isolated handles."
  type        = string
}

variable "data_kms_key_arn" {
  description = "cmk-data, the key the account cost pipeline encrypts Athena results and estimate exports with. The operator needs Decrypt AND GenerateDataKey on it: the workgroup enforces SSE-KMS results, so Decrypt alone leaves every query failing at the write step."
  type        = string
}

variable "tags" {
  description = "Common tags applied to all resources"
  type        = map(string)
  default     = {}
}
