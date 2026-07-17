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

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped IAM/SSM names; 27 is the tightest cluster-scoped budget (an S3 bucket in a sibling component) so every derived name stays within AWS limits."
  }
}

variable "vpc_id" {
  description = "VPC ID from landing-zone network component"
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnet IDs for interface endpoints"
  type        = list(string)
}

variable "route_table_ids" {
  description = "Route table IDs for gateway endpoints (S3, DynamoDB)"
  type        = list(string)
}

variable "cluster_security_group_id" {
  description = "EKS cluster security group ID — added to interface endpoint SG ingress"
  type        = string
}

variable "enable_waf" {
  description = "Attach a WAF WebACL to the agentgateway ALB. ALB ARN read from SSM by name."
  type        = bool
  default     = false
}

variable "create_vpc_endpoints" {
  description = "Create VPC interface + gateway endpoints. Set false when landing-zone already provisions them for this VPC (the typical case when running on top of lz-network with enable_vpc_endpoints = true)."
  type        = bool
  default     = true
}

variable "agentgateway_alb_arn" {
  description = "ALB ARN for the agentgateway public listener (only when enable_waf=true)"
  type        = string
  default     = ""
}

variable "tags" {
  description = "Common tags"
  type        = map(string)
  default     = {}
}
