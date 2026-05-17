data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  prefix = "${var.environment}-${var.cluster_name}-killswitch"
  tags = merge(var.tags, {
    Component = "kill-switch"
    Tier      = "platform"
  })
}

################################################################################
# EventBridge bus + rule
#
# The budget-controller emits a custom event (source = "eks-agent-platform.budget",
# detail-type = "BudgetBreach") when SpendReport.spend >= BudgetPolicy.threshold * 1.20.
# This rule routes the event to the Step Functions state machine.
################################################################################

resource "aws_cloudwatch_event_bus" "killswitch" {
  name = local.prefix
  tags = local.tags
}

resource "aws_cloudwatch_event_archive" "killswitch" {
  name             = local.prefix
  event_source_arn = aws_cloudwatch_event_bus.killswitch.arn
  retention_days   = 365
  description      = "Retain every budget-breach event for compliance"
}

resource "aws_cloudwatch_event_rule" "breach" {
  name           = "${local.prefix}-breach"
  description    = "BudgetPolicy breach >= 120% — fire kill-switch"
  event_bus_name = aws_cloudwatch_event_bus.killswitch.name
  tags           = local.tags

  event_pattern = jsonencode({
    source        = ["eks-agent-platform.budget"]
    "detail-type" = ["BudgetBreach"]
    detail = {
      severity = ["critical"]
    }
  })
}

################################################################################
# Step Functions state machine
################################################################################

resource "aws_iam_role" "sfn" {
  name = "${local.prefix}-sfn"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "states.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "sfn" {
  name = "killswitch-actions"
  role = aws_iam_role.sfn.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "DetachBedrockFromTenant"
        Effect = "Allow"
        Action = [
          "iam:DetachRolePolicy",
          "iam:GetRole",
          "iam:ListAttachedRolePolicies",
          # Tag tenant role with a suspension marker the operator
          # reads on next reconcile. UntagRole is for the recovery
          # flow when ops manually un-suspends (a future state).
          "iam:TagRole",
          "iam:UntagRole"
        ]
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role${var.tenant_iam_path}*"
      },
      {
        Sid      = "NotifyOperator"
        Effect   = "Allow"
        Action   = "events:PutEvents"
        Resource = "*"
      },
      {
        Sid    = "Logging"
        Effect = "Allow"
        Action = [
          "logs:CreateLogDelivery",
          "logs:GetLogDelivery",
          "logs:UpdateLogDelivery",
          "logs:DeleteLogDelivery",
          "logs:ListLogDeliveries",
          "logs:PutResourcePolicy",
          "logs:DescribeResourcePolicies",
          "logs:DescribeLogGroups"
        ]
        Resource = "*"
      }
    ]
  })
}

resource "aws_cloudwatch_log_group" "sfn" {
  name              = "/aws/states/${local.prefix}"
  retention_in_days = 365
  kms_key_id        = var.logs_kms_key_arn
  tags              = local.tags
}

resource "aws_sfn_state_machine" "killswitch" {
  name     = local.prefix
  role_arn = aws_iam_role.sfn.arn
  type     = "STANDARD"
  tags     = local.tags

  logging_configuration {
    log_destination        = "${aws_cloudwatch_log_group.sfn.arn}:*"
    include_execution_data = true
    level                  = "ALL"
  }

  definition = jsonencode({
    Comment = "Kill-switch: detach Bedrock-invoke from the tenant IRSA role, tag the role with a suspension marker the operator reads, notify operator to scale AgentFleets to zero, record event."
    StartAt = "NormalizeInput"
    States = {
      # Defensive default for $.detail.reason. Budget reconciler always
      # publishes 'reason'; this guard handles manually-crafted breach
      # events (replay, ops test) that may omit it. Without the default,
      # TagRoleSuspended's "Value.$": "$.detail.reason" hits a path-
      # resolution error AFTER DetachBedrockPolicy has already removed
      # the baseline policy — leaving the tenant role in an inconsistent
      # state (no Bedrock invoke, but also no suspension tag for the
      # operator to detect).
      NormalizeInput = {
        Type = "Pass"
        Parameters = {
          "detail.$" : "States.JsonMerge(States.StringToJson('{\"reason\":\"budget-exceeded\"}'), $.detail, false)"
        }
        ResultPath = "$"
        Next       = "DetachBedrockPolicy"
      }
      DetachBedrockPolicy = {
        Type     = "Task"
        Resource = "arn:${data.aws_partition.current.partition}:states:::aws-sdk:iam:detachRolePolicy"
        Parameters = {
          # The role name is built from var.tenant_role_name_pattern with
          # the literal '<env>' substituted with var.environment at plan
          # time, and '{}' substituted with $.detail.platformId at runtime
          # via Step Functions States.Format. This pattern is a CONTRACT
          # with the operator's PlatformReconciler — see the variable doc.
          "RoleName.$" = "States.Format('${replace(var.tenant_role_name_pattern, "<env>", var.environment)}', $.detail.platformId)"
          PolicyArn    = var.tenant_baseline_policy_arn
        }
        Retry = [{
          ErrorEquals     = ["States.TaskFailed"]
          IntervalSeconds = 2
          MaxAttempts     = 3
          BackoffRate     = 2.0
        }]
        Next = "TagRoleSuspended"
        Catch = [{
          ErrorEquals = ["States.ALL"]
          Next        = "RecordFailure"
          ResultPath  = "$.error"
        }]
      }
      # Tag the tenant IRSA role with a suspension marker the operator
      # reads on its next reconcile (via iam:GetRole.Tags). Without this
      # the operator's attachBaselineIfMissing helper would notice the
      # detached baseline and reattach it — undoing the kill-switch
      # within minutes.
      TagRoleSuspended = {
        Type     = "Task"
        Resource = "arn:${data.aws_partition.current.partition}:states:::aws-sdk:iam:tagRole"
        Parameters = {
          "RoleName.$" = "States.Format('${replace(var.tenant_role_name_pattern, "<env>", var.environment)}', $.detail.platformId)"
          Tags = [
            { Key = "agents.stxkxs.io/suspended", Value = "true" },
            { Key = "agents.stxkxs.io/suspended-reason", "Value.$" = "$.detail.reason" }
          ]
        }
        Retry = [{
          ErrorEquals     = ["States.TaskFailed"]
          IntervalSeconds = 2
          MaxAttempts     = 3
          BackoffRate     = 2.0
        }]
        ResultPath = null
        Next       = "NotifyOperator"
        Catch = [{
          ErrorEquals = ["States.ALL"]
          Next        = "RecordFailure"
          ResultPath  = "$.error"
        }]
      }
      NotifyOperator = {
        Type     = "Task"
        Resource = "arn:${data.aws_partition.current.partition}:states:::events:putEvents"
        Parameters = {
          Entries = [{
            "Source"       = "eks-agent-platform.killswitch"
            "DetailType"   = "ScaleToZero"
            "EventBusName" = aws_cloudwatch_event_bus.killswitch.name
            "Detail.$"     = "States.JsonToString($.detail)"
          }]
        }
        End = true
      }
      RecordFailure = {
        Type   = "Pass"
        End    = true
        Result = "kill-switch action failed; see CloudWatch + manual recovery required"
      }
    }
  })
}

resource "aws_cloudwatch_event_target" "to_sfn" {
  rule           = aws_cloudwatch_event_rule.breach.name
  event_bus_name = aws_cloudwatch_event_bus.killswitch.name
  target_id      = "sfn"
  arn            = aws_sfn_state_machine.killswitch.arn
  role_arn       = aws_iam_role.eventbridge.arn
}

resource "aws_iam_role" "eventbridge" {
  name = "${local.prefix}-eventbridge"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "eventbridge" {
  name = "start-sfn"
  role = aws_iam_role.eventbridge.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "states:StartExecution"
      Resource = aws_sfn_state_machine.killswitch.arn
    }]
  })
}

################################################################################
# Allow the operator role to PutEvents on the kill-switch bus
################################################################################

resource "aws_cloudwatch_event_bus_policy" "operator_put" {
  event_bus_name = aws_cloudwatch_event_bus.killswitch.name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "OperatorPutEvents"
      Effect    = "Allow"
      Principal = { AWS = var.operator_role_arn }
      Action    = "events:PutEvents"
      Resource  = aws_cloudwatch_event_bus.killswitch.arn
    }]
  })
}

################################################################################
# SSM outputs
################################################################################

resource "aws_ssm_parameter" "event_bus_name" {
  name  = "/eks-agent-platform/${var.environment}/kill-switch/event_bus_name"
  type  = "String"
  value = aws_cloudwatch_event_bus.killswitch.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "event_bus_arn" {
  name  = "/eks-agent-platform/${var.environment}/kill-switch/event_bus_arn"
  type  = "String"
  value = aws_cloudwatch_event_bus.killswitch.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "state_machine_arn" {
  name  = "/eks-agent-platform/${var.environment}/kill-switch/state_machine_arn"
  type  = "String"
  value = aws_sfn_state_machine.killswitch.arn
  tags  = local.tags
}
