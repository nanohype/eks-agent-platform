output "endpoint_security_group_id" {
  description = "Security group on the VPC interface endpoints"
  value       = aws_security_group.endpoints.id
}

output "interface_endpoint_ids" {
  description = "Map of service name → interface endpoint ID"
  value       = { for s, e in aws_vpc_endpoint.interface : s => e.id }
}

output "gateway_endpoint_ids" {
  description = "Map of service name → gateway endpoint ID"
  value       = { for s, e in aws_vpc_endpoint.gateway : s => e.id }
}

output "waf_web_acl_arn" {
  description = "ARN of the WAF WebACL protecting agentgateway (null if disabled)"
  value       = var.enable_waf ? aws_wafv2_web_acl.agentgateway[0].arn : null
}
