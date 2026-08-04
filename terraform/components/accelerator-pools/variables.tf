variable "cluster_name" {
  description = "EKS cluster name"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped IAM/SSM names; 27 is the tightest cluster-scoped budget (an S3 bucket in a sibling component) so every derived name stays within AWS limits."
  }
}

# node_role_name is deliberately absent. The only thing this component ever put on
# the Karpenter node role was an ec2:Describe* policy for the Neuron device
# plugin's topology discovery, and there is no Neuron device plugin. The GPU
# Operator's node-side needs are covered by the AWS EKS-managed node role
# baseline, so this component no longer reaches the node role at all.

variable "gpu_operator_namespace" {
  description = "Namespace where the NVIDIA GPU Operator runs (matches the eks-gitops addons-accelerators-helm ApplicationSet)"
  type        = string
  default     = "gpu-operator"
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
