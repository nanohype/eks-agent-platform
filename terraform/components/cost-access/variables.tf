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

# There is deliberately no data_kms_key_arn variable.
#
# The operator needs Decrypt AND GenerateDataKey on the key the account pipeline
# encrypts Athena results and estimate exports with — the workgroup enforces SSE-KMS
# results, so Decrypt alone leaves every query failing at the write step. But which key
# that is, is not this component's to decide: it is read from the account contract at
# /eks-agent-platform/org/cost-pipeline/data_kms_key_arn.
#
# As an input it was a second place the same value was set, with nothing comparing the
# two. A grant scoped to a key the workgroup does not use fails on the WRITE, which the
# reconciler reports as an unreadable spend and not as an access error, so every budget
# goes stale with nothing red.

variable "tags" {
  description = "Common tags applied to all resources"
  type        = map(string)
  default     = {}
}
