variable "environment" {
  description = "Environment name"
  type        = string
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name"
  type        = string
}

variable "oidc_provider_arn" {
  description = "EKS OIDC provider ARN (from landing-zone cluster component)"
  type        = string
}

variable "oidc_issuer" {
  description = "EKS OIDC issuer URL (https://oidc.eks.<region>.amazonaws.com/id/<id>)"
  type        = string
}

variable "operator_namespace" {
  description = "Namespace where the operator runs"
  type        = string
  default     = "eks-agent-platform"
}

variable "operator_service_account" {
  description = "ServiceAccount the operator pod uses"
  type        = string
  default     = "operator"
}

variable "tenant_iam_path" {
  description = "IAM path under which all per-tenant roles live. The operator's role is scoped to this path."
  type        = string
  default     = "/eks-agent-platform/tenants/"
}

variable "data_kms_key_arn" {
  description = "cmk-data ARN — operator needs CreateGrant on this for per-tenant grants"
  type        = string
}

variable "artifacts_bucket_arn" {
  description = "Model artifacts bucket ARN — operator needs to scope tenant access to subpaths"
  type        = string
}

variable "allowed_regions" {
  description = "AWS regions tenant roles are permitted to invoke Bedrock in. Used as the aws:RequestedRegion condition on the baseline policy; the operator narrows further per-Platform."
  type        = list(string)
  default     = ["us-east-1", "us-west-2"]
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
