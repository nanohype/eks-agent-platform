data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  prefix = "${var.environment}-${var.cluster_name}-batchrunner"
  tags = merge(var.tags, {
    Component = "batch-runtime"
    Tier      = "platform"
  })
  # Batch input/output JSONL is staged under this prefix of the shared
  # model-artifacts bucket; the service role's S3 access is scoped to it.
  batch_prefix = "batch"
}

################################################################################
# Bedrock batch service role
#
# Amazon Bedrock assumes this role to run a batch model-invocation job
# (CreateModelInvocationJob): it reads the input JSONL from S3, runs the
# model, and writes the output JSONL back. It is distinct from the operator
# IRSA (agent-iam) that *submits* the job — the operator passes this role's
# ARN as the job's RoleArn (gated by iam:PassedToService=bedrock.amazonaws.com).
#
# Trust is scoped with aws:SourceAccount + aws:SourceArn (model-invocation-job)
# per the AWS batch-inference service-role guidance:
# https://docs.aws.amazon.com/bedrock/latest/userguide/batch-iam-sr.html
################################################################################

resource "aws_iam_role" "batch" {
  name = local.prefix
  path = "/eks-agent-platform/"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "bedrock.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        StringEquals = {
          "aws:SourceAccount" = data.aws_caller_identity.current.account_id
        }
        ArnLike = {
          "aws:SourceArn" = "arn:${data.aws_partition.current.partition}:bedrock:${var.region}:${data.aws_caller_identity.current.account_id}:model-invocation-job/*"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "batch" {
  name = "batch-data-access"
  role = aws_iam_role.batch.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ReadWriteBatchPrefix"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = ["${var.artifacts_bucket_arn}/${local.batch_prefix}/*"]
      },
      {
        Sid      = "ListBatchPrefix"
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = [var.artifacts_bucket_arn]
        Condition = {
          StringLike = {
            "s3:prefix" = ["${local.batch_prefix}/*"]
          }
        }
      },
      {
        Sid      = "BatchDataKMS"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
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
# SSM output — the operator resolves the service-role ARN at startup and
# passes it as the BatchJob's RoleArn.
################################################################################

resource "aws_ssm_parameter" "service_role_arn" {
  name  = "/eks-agent-platform/${var.environment}/batch-runtime/service_role_arn"
  type  = "String"
  value = aws_iam_role.batch.arn
  tags  = local.tags
}
