variable "cluster_name" {
  description = "EKS cluster name"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped IAM/SSM names; 27 is the tightest cluster-scoped budget (an S3 bucket in a sibling component) so every derived name stays within AWS limits."
  }
}

variable "node_role_name" {
  description = "Existing Karpenter node IAM role name (from landing-zone cluster). Operator extends this with accelerator-specific permissions."
  type        = string
}

variable "neuron_addon_namespace" {
  description = "Namespace where the AWS Neuron device plugin runs (matches the eks-gitops aws-neuron-device-plugin addon)"
  type        = string
  default     = "aws-neuron"
}

variable "gpu_operator_namespace" {
  description = "Namespace where the NVIDIA GPU Operator runs"
  type        = string
  default     = "gpu-operator"
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
