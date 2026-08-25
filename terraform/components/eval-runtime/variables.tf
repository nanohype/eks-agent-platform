variable "region" {
  description = "AWS region"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name — used to namespace SSM parameters and tags"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped IAM/SSM names; 27 is the tightest cluster-scoped budget (an S3 bucket in a sibling component) so every derived name stays within AWS limits."
  }
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

variable "bedrock_invoke_resource_arns" {
  description = <<-EOT
    Bedrock model ARNs eval-runner pods may invoke. Required: there is no default,
    because the only default this variable could carry is a permissive one.

    "*" here is a path around the per-Platform model scoping the operator
    generates — an eval-runner pod holding it can invoke every model in the
    account regardless of what any Platform's allowedModels says. A default that
    grants that silently makes the open case the one you get by not deciding,
    which inverts the direction an operator expects: whoever leaves an allowlist
    alone believes they have granted nothing.

    Pass the specific cross-region inference-profile ARNs the eval suites
    exercise. In a development environment that list is still a list, not "*".
  EOT
  type        = list(string)

  validation {
    condition     = length(var.bedrock_invoke_resource_arns) > 0
    error_message = "bedrock_invoke_resource_arns must name at least one ARN; an empty list would render a policy granting nothing and the eval runner would fail every case with AccessDenied."
  }

  validation {
    condition     = !contains(var.bedrock_invoke_resource_arns, "*")
    error_message = "bedrock_invoke_resource_arns must not contain \"*\" — it grants invoke on every model in the account and bypasses the per-Platform model scoping the operator generates. Name the inference-profile ARNs the eval suites actually exercise."
  }
}

variable "allowed_regions" {
  description = "Bedrock regions eval-runner pods may invoke in (aws:RequestedRegion ABAC). Matches the convention from agent-iam — non-taggable model resources are constrained via region."
  type        = list(string)
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

variable "data_kms_key_arn" {
  description = "The platform data CMK ARN — eval-runner pods get kms:Decrypt scoped via kms:ViaService for the eval-reports bucket reads."
  type        = string
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
