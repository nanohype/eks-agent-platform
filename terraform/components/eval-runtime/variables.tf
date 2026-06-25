variable "environment" {
  description = "Environment name (dev, staging, production)"
  type        = string
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name — used to namespace SSM parameters and tags"
  type        = string
}

variable "eval_runner_namespace" {
  description = "Kubernetes namespace where Argo Workflows execute. Argo Workflows must already be installed (via the eks-gitops addons-argo-platform ApplicationSet)."
  type        = string
  default     = "eval-runner"
}

variable "eval_runner_service_account" {
  description = "ServiceAccount Argo Workflow pods assume. The reconciler emits Workflows referencing this SA name."
  type        = string
  default     = "eval-runner"
}

variable "eval_reports_bucket_arn" {
  description = "Eval reports S3 bucket ARN (from model-artifacts component) — eval-runner pods get scoped read+write here for HTML report uploads."
  type        = string
}

variable "eval_reports_bucket_name" {
  description = "Eval reports S3 bucket name (from model-artifacts component) — published to SSM for the reconciler."
  type        = string
}

variable "bedrock_invoke_resource_arns" {
  description = "List of Bedrock model ARNs eval-runner pods can invoke. Defaults to '*' which is fine in dev; production should pass the specific cross-region inference profile ARNs the eval suites actually exercise."
  type        = list(string)
  default     = ["*"]
}

variable "allowed_regions" {
  description = "Bedrock regions eval-runner pods may invoke in (aws:RequestedRegion ABAC). Matches the convention from agent-iam — non-taggable model resources are constrained via region."
  type        = list(string)
}

variable "logs_kms_key_arn" {
  description = "KMS key ARN for encrypting eval-runner CloudWatch log group (cmk-logs)"
  type        = string
}

variable "log_retention_days" {
  description = "How long to retain eval-runner Workflow controller logs in CloudWatch"
  type        = number
  default     = 90
}

variable "data_kms_key_arn" {
  description = "cmk-data ARN — eval-runner pods get kms:Decrypt scoped via EncryptionContext for the eval-reports bucket reads."
  type        = string
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
