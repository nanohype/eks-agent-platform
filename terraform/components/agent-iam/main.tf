data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  prefix = "${var.environment}-${var.cluster_name}-eap"
  tags = merge(var.tags, {
    Component = "agent-iam"
    Tier      = "platform"
  })

  oidc_host = replace(var.oidc_issuer, "https://", "")
}

################################################################################
# Operator IRSA role
#
# The operator pod assumes this role and uses it to provision per-tenant IAM
# resources at reconcile time. It is the only role with iam:* under the tenant
# path — tenants themselves never have IAM permissions.
################################################################################

resource "aws_iam_role" "operator" {
  name = "${local.prefix}-operator"
  path = "/eks-agent-platform/"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = var.oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_host}:sub" = "system:serviceaccount:${var.operator_namespace}:${var.operator_service_account}"
          "${local.oidc_host}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "operator_iam" {
  name = "tenant-iam-management"
  role = aws_iam_role.operator.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "ManageTenantRoles"
        Effect = "Allow"
        Action = [
          "iam:CreateRole",
          "iam:DeleteRole",
          "iam:GetRole",
          "iam:ListRolePolicies",
          "iam:ListAttachedRolePolicies",
          "iam:PutRolePolicy",
          "iam:DeleteRolePolicy",
          "iam:AttachRolePolicy",
          "iam:DetachRolePolicy",
          "iam:TagRole",
          "iam:UntagRole",
          "iam:UpdateAssumeRolePolicy"
        ]
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role${var.tenant_iam_path}*"
      },
      {
        Sid    = "ManageTenantPolicies"
        Effect = "Allow"
        Action = [
          "iam:CreatePolicy",
          "iam:CreatePolicyVersion",
          "iam:DeletePolicy",
          "iam:DeletePolicyVersion",
          "iam:GetPolicy",
          "iam:GetPolicyVersion",
          "iam:ListPolicyVersions",
          "iam:ListEntitiesForPolicy"
        ]
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:policy${var.tenant_iam_path}*"
      },
      {
        Sid      = "PassTenantRolesToBedrock"
        Effect   = "Allow"
        Action   = "iam:PassRole"
        Resource = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role${var.tenant_iam_path}*"
        Condition = {
          StringEquals = {
            "iam:PassedToService" = ["bedrock.amazonaws.com"]
          }
        }
      }
    ]
  })
}

resource "aws_iam_role_policy" "operator_kms" {
  name = "tenant-kms-grants"
  role = aws_iam_role.operator.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "kms:CreateGrant",
        "kms:RevokeGrant",
        "kms:ListGrants",
        "kms:DescribeKey"
      ]
      Resource = var.data_kms_key_arn
      Condition = {
        Bool = {
          "kms:GrantIsForAWSResource" = "true"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "operator_artifacts" {
  name = "artifacts-bucket-management"
  role = aws_iam_role.operator.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "s3:GetBucketPolicy",
        "s3:PutBucketPolicy"
      ]
      Resource = var.artifacts_bucket_arn
    }]
  })
}

resource "aws_iam_role_policy" "operator_bedrock_introspection" {
  name = "bedrock-introspection"
  role = aws_iam_role.operator.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "bedrock:ListFoundationModels",
        "bedrock:GetFoundationModel",
        "bedrock:ListInferenceProfiles",
        "bedrock:GetInferenceProfile",
        "bedrock:ListGuardrails",
        "bedrock:GetGuardrail"
      ]
      Resource = "*"
    }]
  })
}

################################################################################
# Tenant role template — the operator stamps these out per Platform CR.
# We export the assume-role-policy template + the policy-template ARNs as SSM
# so the controller doesn't have to rebuild them from scratch.
################################################################################

resource "aws_iam_policy" "tenant_baseline" {
  name = "${local.prefix}-tenant-baseline"
  path = var.tenant_iam_path
  tags = local.tags

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # Bedrock foundation-model ARNs are AWS-managed and cannot be tagged,
        # so ABAC via aws:ResourceTag/PlatformId silently denies every call.
        # The operator scopes tenant access by ARN at reconcile time —
        # one statement per allowed model in Platform.spec.identity, with
        # Resource set to the actual model ARNs (or cross-region inference
        # profile ARNs, which ARE per-account and CAN be tagged if needed).
        # This baseline grants the action but the per-tenant policy generated
        # by the operator narrows Resource. The aws:RequestedRegion condition
        # contains the blast radius if the operator mis-renders a tenant
        # policy.
        Sid    = "BedrockInvoke"
        Effect = "Allow"
        Action = [
          "bedrock:InvokeModel",
          "bedrock:InvokeModelWithResponseStream",
          "bedrock:Converse",
          "bedrock:ConverseStream",
          "bedrock:ApplyGuardrail"
        ]
        Resource = "*"
        Condition = {
          StringEquals = {
            "aws:RequestedRegion" = var.allowed_regions
          }
        }
      },
      {
        Sid    = "CloudWatchLogs"
        Effect = "Allow"
        Action = [
          "logs:CreateLogStream",
          "logs:PutLogEvents"
        ]
        Resource = "arn:${data.aws_partition.current.partition}:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:/eks-agent-platform/${var.environment}/tenants/*"
      },
      {
        # KEDA aws-sqs-queue trigger uses the tenant SA's IRSA token to
        # GetQueueAttributes for ApproximateNumberOfMessages. Read-only,
        # region-scoped so a misconfigured queue URL in another region
        # can't be peeked at. Resource '*' is intentional — the queue
        # ARN comes from AgentFleet.spec.scaling.queueUrl which the
        # tenant controls; we'd otherwise need a per-fleet inline policy
        # to scope this, at which point the kill-switch contract gets
        # harder to maintain. The region constraint plus read-only
        # action keeps the blast radius small.
        Sid    = "KEDASQSScalerRead"
        Effect = "Allow"
        Action = [
          "sqs:GetQueueAttributes",
          "sqs:ListQueueTags"
        ]
        Resource = "*"
        Condition = {
          StringEquals = {
            "aws:RequestedRegion" = var.allowed_regions
          }
        }
      }
    ]
  })
}

################################################################################
# SSM outputs
################################################################################

resource "aws_ssm_parameter" "operator_role_arn" {
  name  = "/eks-agent-platform/${var.environment}/agent-iam/operator_role_arn"
  type  = "String"
  value = aws_iam_role.operator.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "tenant_iam_path" {
  name  = "/eks-agent-platform/${var.environment}/agent-iam/tenant_iam_path"
  type  = "String"
  value = var.tenant_iam_path
  tags  = local.tags
}

resource "aws_ssm_parameter" "tenant_baseline_policy_arn" {
  name  = "/eks-agent-platform/${var.environment}/agent-iam/tenant_baseline_policy_arn"
  type  = "String"
  value = aws_iam_policy.tenant_baseline.arn
  tags  = local.tags
}
