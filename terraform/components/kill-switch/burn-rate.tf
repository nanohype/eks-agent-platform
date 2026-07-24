################################################################################
# Burn-rate breach routing
#
# The SLO reconciler publishes source = "governance.nanohype.dev/slo",
# detail-type = "BurnRateBreach" when both windows of a burn-rate pair trip on a
# tenant's SLOPolicy. These rules route it to the severity topic the
# observability-slo standard's fleet_alerting tiers name: critical pages,
# warning tickets.
#
# The event shares this bus with the budget kill-switch but must never reach the
# suspension state machine — a burn-rate breach holds a tenant's GitOps delivery,
# it does not detach IAM. EventBridge evaluates every rule independently and
# matches `source` exactly, so the disjoint source strings are the mechanical
# guarantee the two rule sets cannot cross-match. A Go contract test
# (slo_killswitch_contract_test.go) parses these patterns, asserts they equal the
# operator's constants, and asserts they stay disjoint from the budget rule's.
#
# Kept in its own file rather than appended to main.tf so each rule's contract
# test can scope its regexes to one file, and so adding a rule can never
# retarget the other test by landing textually first.
#
# The cluster's severity topics come from landing-zone's observability component,
# read through the same SSM contract this component already uses for agent-iam.
# The archive on the bus (main.tf) retains these events alongside budget
# breaches — it is bus-scoped, not rule-scoped.
################################################################################

data "aws_ssm_parameter" "alerts_critical_topic_arn" {
  name = "/eks-agent-platform/${var.cluster_name}/observability/alerts_critical_topic_arn"
}

data "aws_ssm_parameter" "alerts_warning_topic_arn" {
  name = "/eks-agent-platform/${var.cluster_name}/observability/alerts_warning_topic_arn"
}

resource "aws_cloudwatch_event_rule" "burn_rate_critical" {
  name           = "${local.prefix}-burnrate-critical"
  description    = "SLOPolicy page-tier burn-rate breach — page"
  event_bus_name = aws_cloudwatch_event_bus.killswitch.name
  tags           = local.tags

  event_pattern = jsonencode({
    source        = ["governance.nanohype.dev/slo"]
    "detail-type" = ["BurnRateBreach"]
    detail = {
      severity = ["critical"]
    }
  })
}

resource "aws_cloudwatch_event_rule" "burn_rate_warning" {
  name           = "${local.prefix}-burnrate-warning"
  description    = "SLOPolicy ticket-tier burn-rate breach — ticket"
  event_bus_name = aws_cloudwatch_event_bus.killswitch.name
  tags           = local.tags

  event_pattern = jsonencode({
    source        = ["governance.nanohype.dev/slo"]
    "detail-type" = ["BurnRateBreach"]
    detail = {
      severity = ["warning"]
    }
  })
}

# No role_arn on either target: EventBridge publishes to SNS through the topic's
# own resource policy, not an assumed role. The observability component grants
# events.amazonaws.com both SNS:Publish on the topic and kms:GenerateDataKey* on
# the topics' CMK — the topics are SSE-KMS, so without the key grant the publish
# is accepted and then silently dropped.
resource "aws_cloudwatch_event_target" "burn_rate_critical_to_sns" {
  rule           = aws_cloudwatch_event_rule.burn_rate_critical.name
  event_bus_name = aws_cloudwatch_event_bus.killswitch.name
  target_id      = "sns-critical"
  arn            = data.aws_ssm_parameter.alerts_critical_topic_arn.value
}

resource "aws_cloudwatch_event_target" "burn_rate_warning_to_sns" {
  rule           = aws_cloudwatch_event_rule.burn_rate_warning.name
  event_bus_name = aws_cloudwatch_event_bus.killswitch.name
  target_id      = "sns-warning"
  arn            = data.aws_ssm_parameter.alerts_warning_topic_arn.value
}
