locals {
  prefix = "${var.environment}-${var.cluster_name}-egress"
  tags = merge(var.tags, {
    Component = "agent-egress"
    Tier      = "platform"
  })

  interface_services = [
    "bedrock-runtime",
    "bedrock",
    "bedrock-agent-runtime",
    "sts",
    "secretsmanager",
    "logs",
    "monitoring",
    "ssm",
    "kms"
  ]

  gateway_services = [
    "s3",
    "dynamodb"
  ]
}

################################################################################
# Endpoint SG — allows TCP/443 from anything in the cluster SG
################################################################################

resource "aws_security_group" "endpoints" {
  name        = "${local.prefix}-endpoints"
  description = "Allow EKS cluster to reach VPC interface endpoints over 443"
  vpc_id      = var.vpc_id
  tags        = local.tags
}

resource "aws_vpc_security_group_ingress_rule" "from_cluster" {
  security_group_id            = aws_security_group.endpoints.id
  description                  = "EKS cluster to interface endpoints"
  from_port                    = 443
  to_port                      = 443
  ip_protocol                  = "tcp"
  referenced_security_group_id = var.cluster_security_group_id
  tags                         = local.tags
}

# No egress rules — interface endpoint ENIs do not initiate outbound traffic.
# Response packets to ingress are stateful and do not require an egress rule.

################################################################################
# Interface VPC endpoints
################################################################################

resource "aws_vpc_endpoint" "interface" {
  for_each = toset(local.interface_services)

  vpc_id              = var.vpc_id
  service_name        = "com.amazonaws.${var.region}.${each.key}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = var.private_subnet_ids
  security_group_ids  = [aws_security_group.endpoints.id]
  private_dns_enabled = true

  tags = merge(local.tags, { Endpoint = each.key })
}

################################################################################
# Gateway endpoints (no SG, attached to route tables)
################################################################################

resource "aws_vpc_endpoint" "gateway" {
  for_each = toset(local.gateway_services)

  vpc_id            = var.vpc_id
  service_name      = "com.amazonaws.${var.region}.${each.key}"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = var.route_table_ids

  tags = merge(local.tags, { Endpoint = each.key })
}

################################################################################
# WAF for the agentgateway ALB (opt-in)
################################################################################

resource "aws_wafv2_web_acl" "agentgateway" {
  count = var.enable_waf ? 1 : 0

  name        = "${local.prefix}-agentgateway"
  description = "Protects agentgateway public listener"
  scope       = "REGIONAL"

  default_action {
    allow {}
  }

  rule {
    name     = "AWSManagedRulesCommonRuleSet"
    priority = 0
    override_action {
      none {}
    }
    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesCommonRuleSet"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "common-ruleset"
      sampled_requests_enabled   = true
    }
  }

  rule {
    name     = "AWSManagedRulesKnownBadInputsRuleSet"
    priority = 1
    override_action {
      none {}
    }
    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesKnownBadInputsRuleSet"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "known-bad-inputs"
      sampled_requests_enabled   = true
    }
  }

  rule {
    name     = "RateLimit"
    priority = 10
    action {
      block {}
    }
    statement {
      rate_based_statement {
        limit              = 2000
        aggregate_key_type = "IP"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "rate-limit"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${local.prefix}-agentgateway"
    sampled_requests_enabled   = true
  }

  tags = local.tags
}

resource "aws_wafv2_web_acl_association" "agentgateway" {
  count        = var.enable_waf ? 1 : 0
  resource_arn = var.agentgateway_alb_arn
  web_acl_arn  = aws_wafv2_web_acl.agentgateway[0].arn
}

################################################################################
# SSM outputs
################################################################################

resource "aws_ssm_parameter" "endpoint_sg_id" {
  name  = "/eks-agent-platform/${var.environment}/agent-egress/endpoint_sg_id"
  type  = "String"
  value = aws_security_group.endpoints.id
  tags  = local.tags
}
