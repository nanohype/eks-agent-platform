locals {
  prefix = "${var.environment}-${var.cluster_name}-evalrunner"
  tags = merge(var.tags, {
    Component = "eval-runtime"
    Tier      = "platform"
  })
}

################################################################################
# IRSA role for the eval-runner ServiceAccount
#
# Argo Workflow pods spawned by the operator's eval Workflow / CronWorkflow CRs
# (see operators/internal/controller/eval_reconcile.go) run under this role.
# They need:
#   - bedrock:InvokeModel — to drive the agent under test
#   - s3:PutObject on eval-reports/{platform}/runs/{suiteName}/{runId}/* — HTML
#     + junit.xml artifacts
#   - s3:GetObject on eval-reports/{platform}/manifests/* — when EvalSuite.spec
#     .casesFromManifest points at a manifest JSON
#   - kms:Decrypt on cmk-data — eval-reports is SSE-KMS-encrypted
#
# Region access is constrained via aws:RequestedRegion to match the agent-iam
# baseline policy convention (model ARNs aren't taggable).
################################################################################

resource "aws_iam_role" "eval_runner" {
  name = local.prefix
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
    }]
  })
}

# EKS Pod Identity binds this role to the eval-runner ServiceAccount — no IRSA
# annotation, no OIDC trust.
resource "aws_eks_pod_identity_association" "eval_runner" {
  cluster_name    = var.cluster_name
  namespace       = var.eval_runner_namespace
  service_account = var.eval_runner_service_account
  role_arn        = aws_iam_role.eval_runner.arn
  tags            = local.tags
}

resource "aws_iam_role_policy" "eval_runner" {
  name = "eval-runner-actions"
  role = aws_iam_role.eval_runner.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "InvokeBedrock"
        Effect   = "Allow"
        Action   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
        Resource = var.bedrock_invoke_resource_arns
        Condition = {
          StringEquals = {
            "aws:RequestedRegion" = var.allowed_regions
          }
        }
      },
      {
        # Per-platform path-scoped object writes. The eval-runner
        # workflow template uploads HTML + junit.xml under
        # eval-reports/<platform>/runs/<suite>/<runId>/*; restrict the
        # IAM policy to that prefix so a compromised eval-runner pod
        # can't overwrite another tenant's reports or manifests. The
        # bucket is shared across platforms (one S3 bucket from
        # model-artifacts) so this is the only multi-tenant boundary
        # at the IAM layer.
        Sid      = "WriteEvalReports"
        Effect   = "Allow"
        Action   = ["s3:PutObject", "s3:AbortMultipartUpload"]
        Resource = ["${var.eval_reports_bucket_arn}/*/runs/*"]
      },
      {
        # ListBucketMultipartUploads is a bucket-level action; put it
        # on the bucket ARN (no /* suffix) or AWS silently denies it.
        # Used by the AWS CLI when an upload is resumed across pod
        # restarts.
        Sid      = "ListMultipartUploadsBucket"
        Effect   = "Allow"
        Action   = ["s3:ListBucketMultipartUploads"]
        Resource = [var.eval_reports_bucket_arn]
      },
      {
        Sid      = "ReadEvalManifests"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:GetObjectVersion"]
        Resource = ["${var.eval_reports_bucket_arn}/*/manifests/*"]
      },
      {
        Sid      = "ListEvalReports"
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = [var.eval_reports_bucket_arn]
      },
      {
        Sid    = "DecryptEvalData"
        Effect = "Allow"
        Action = [
          "kms:Decrypt",
          "kms:GenerateDataKey",
          "kms:DescribeKey"
        ]
        Resource = [var.data_kms_key_arn]
        Condition = {
          StringEquals = {
            "kms:ViaService" = ["s3.${var.region}.amazonaws.com"]
          }
        }
      }
    ]
  })
}

################################################################################
# Argo Workflows controller logs — separate from per-Workflow pod logs so the
# controller-level errors (template parse failures, scheduling decisions) have
# their own retention policy.
################################################################################

resource "aws_cloudwatch_log_group" "controller" {
  name              = "/aws/eks/${var.cluster_name}/eval-runner"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.logs_kms_key_arn
  tags              = local.tags
}

################################################################################
# SSM outputs — picked up by the operator at startup (see
# operators/internal/operatorconfig/config.go) and consumed by the reconciler
# emit step that needs the role ARN for the eval-runner ServiceAccount
# annotation.
################################################################################

resource "aws_ssm_parameter" "eval_runner_role_arn" {
  name  = "/eks-agent-platform/${var.environment}/eval-runtime/runner_role_arn"
  type  = "String"
  value = aws_iam_role.eval_runner.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "eval_runner_namespace" {
  name  = "/eks-agent-platform/${var.environment}/eval-runtime/runner_namespace"
  type  = "String"
  value = var.eval_runner_namespace
  tags  = local.tags
}

resource "aws_ssm_parameter" "eval_runner_service_account" {
  name  = "/eks-agent-platform/${var.environment}/eval-runtime/runner_service_account"
  type  = "String"
  value = var.eval_runner_service_account
  tags  = local.tags
}

resource "aws_ssm_parameter" "eval_reports_bucket" {
  name  = "/eks-agent-platform/${var.environment}/eval-runtime/eval_reports_bucket"
  type  = "String"
  value = var.eval_reports_bucket_name
  tags  = local.tags
}
