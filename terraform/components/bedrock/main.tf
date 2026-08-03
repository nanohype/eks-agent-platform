locals {
  prefix = "${var.cluster_name}-bedrock"
  tags = merge(var.tags, {
    Component = "bedrock"
    Tier      = "platform"
  })
}

################################################################################
# Baseline Guardrail
#
# Tenants override or extend per route via ModelGateway.spec.routes[].guardrailRef
# (operator-reconciled); this baseline is the account-wide default.
#
# Region availability: Bedrock Guardrails are available in a subset of regions
# (us-east-1, us-west-2, eu-central-1, ap-northeast-1, ap-southeast-1, +
# growing). Applying this component in an unsupported region fails with
# 'Service Bedrock Guardrails is not available in this region'. The region
# check below short-circuits with a clear error before the API call.
################################################################################

data "aws_region" "current" {}

locals {
  guardrail_supported_regions = [
    "us-east-1",
    "us-west-2",
    "eu-central-1",
    "eu-west-1",
    "eu-west-3",
    "ap-northeast-1",
    "ap-southeast-1",
    "ap-southeast-2",
  ]
  # Only enable the guardrail when the user-set toggle is true AND the region
  # supports Bedrock Guardrails. SSM output and consumers (the operator, and
  # ModelGateway routes) handle baseline_guardrail_id being null gracefully.
  enable_guardrail = var.enable_guardrails_baseline && contains(local.guardrail_supported_regions, data.aws_region.current.region)
}

resource "aws_bedrock_guardrail" "baseline" {
  count = local.enable_guardrail ? 1 : 0

  name                      = "${local.prefix}-baseline"
  description               = "Platform baseline — denied topics + PII redaction. Tenants extend per route via ModelGateway guardrailRef."
  blocked_input_messaging   = "I can't help with that."
  blocked_outputs_messaging = "I'm not able to share that."

  content_policy_config {
    filters_config {
      input_strength  = "HIGH"
      output_strength = "HIGH"
      type            = "SEXUAL"
    }
    filters_config {
      input_strength  = "HIGH"
      output_strength = "HIGH"
      type            = "VIOLENCE"
    }
    filters_config {
      input_strength  = "HIGH"
      output_strength = "HIGH"
      type            = "HATE"
    }
    filters_config {
      input_strength  = "HIGH"
      output_strength = "HIGH"
      type            = "INSULTS"
    }
    filters_config {
      input_strength  = "MEDIUM"
      output_strength = "MEDIUM"
      type            = "MISCONDUCT"
    }
    filters_config {
      input_strength  = "HIGH"
      output_strength = "NONE"
      type            = "PROMPT_ATTACK"
    }
  }

  sensitive_information_policy_config {
    pii_entities_config {
      action = "ANONYMIZE"
      type   = "EMAIL"
    }
    pii_entities_config {
      action = "ANONYMIZE"
      type   = "PHONE"
    }
    pii_entities_config {
      action = "ANONYMIZE"
      type   = "CREDIT_DEBIT_CARD_NUMBER"
    }
    pii_entities_config {
      action = "BLOCK"
      type   = "US_SOCIAL_SECURITY_NUMBER"
    }
  }

  tags = local.tags
}

################################################################################
# The account's invocation logging is NOT republished here.
#
# The logging configuration, its bucket and its log group are account+region
# singletons owned by bedrock-account, and this component is applied per cluster —
# so the republish shape would be the right one if anything on a cluster prefix
# needed them. Nothing does.
#
# A cluster-prefix parameter exists for exactly one reader: the operator's recursive
# GetParametersByPath sweep of /eks-agent-platform/<cluster-name>/. The operator
# neither reads Bedrock invocation logs nor addresses that bucket — it has no
# CloudWatch Logs client at all — so both keys were created, tagged and destroyed
# per cluster for nobody.
#
# The log group's real consumer is cost-pipeline, which subscribes its cost publisher
# to it and reads it from the account contract directly, at
# /eks-agent-platform/org/bedrock-account/invocation_log_group. That is the account
# root reading an account value: no cluster hop, nothing to keep in sync.
################################################################################

################################################################################
# Guardrails — genuinely per-cluster. A guardrail is a named resource, so many can
# exist in one account and each cluster gets its own.
################################################################################

resource "aws_ssm_parameter" "baseline_guardrail_id" {
  count = local.enable_guardrail ? 1 : 0
  name  = "/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id"
  type  = "String"
  value = aws_bedrock_guardrail.baseline[0].guardrail_id
  tags  = local.tags
}

# The version, alongside the id (operatorconfig: bedrock/baseline_guardrail_version).
# Both, because a guardrail is versioned and the version is what an invocation pins —
# a consumer holding the id alone cannot name what it is applying. The ModelGateway
# reconciler takes the pair as the default for a route that names no guardrail of
# its own.
resource "aws_ssm_parameter" "baseline_guardrail_version" {
  count = local.enable_guardrail ? 1 : 0
  name  = "/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_version"
  type  = "String"
  value = aws_bedrock_guardrail.baseline[0].version
  tags  = local.tags
}
