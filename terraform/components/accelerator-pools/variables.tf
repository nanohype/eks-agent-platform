variable "environment" {
  description = "Environment name"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name"
  type        = string
}

variable "oidc_provider_arn" {
  description = "EKS OIDC provider ARN"
  type        = string
}

variable "oidc_issuer" {
  description = "EKS OIDC issuer URL"
  type        = string
}

variable "node_role_name" {
  description = "Existing Karpenter node IAM role name (from landing-zone cluster). Operator extends this with accelerator-specific permissions."
  type        = string
}

variable "neuron_addon_namespace" {
  description = "Namespace where the AWS Neuron device plugin runs"
  type        = string
  default     = "kube-system"
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
