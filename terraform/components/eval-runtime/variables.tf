variable "cluster_name" {
  description = "EKS cluster name — used to namespace SSM parameters and tags"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped IAM/SSM names; 27 is the tightest cluster-scoped budget (an S3 bucket in a sibling component) so every derived name stays within AWS limits."
  }
}

variable "eval_runner_namespace" {
  description = "Kubernetes namespace eval Workflows run in, and where the operator creates each Platform's eval ServiceAccount. Must equal the operator chart's evalRuntime.namespace, which installs the eval-runner WorkflowTemplate there. Argo Workflows must already be installed (via the eks-gitops addons-argo-platform ApplicationSet)."
  type        = string
  default     = "eval-runner"
}

variable "logs_kms_key_arn" {
  description = "KMS key ARN for encrypting the eval-runner CloudWatch log group — landing-zone's log-path key."
  type        = string
}

variable "log_retention_days" {
  description = "How long to retain eval-runner Workflow controller logs in CloudWatch"
  type        = number
  default     = 90
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
