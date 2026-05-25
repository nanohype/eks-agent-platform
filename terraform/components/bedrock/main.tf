data "aws_caller_identity" "current" {}

locals {
  prefix = "${var.environment}-${var.cluster_name}-bedrock"
  tags = merge(var.tags, {
    Component = "bedrock"
    Tier      = "platform"
  })
}

################################################################################
# Access-logs bucket — receives server-access logs from the invocations
# bucket. Kept separate so audit access can be scoped tightly.
################################################################################

resource "aws_s3_bucket" "access_logs" {
  bucket = "${local.prefix}-access-logs-${data.aws_caller_identity.current.account_id}"
  tags   = local.tags
}

resource "aws_s3_bucket_public_access_block" "access_logs" {
  bucket                  = aws_s3_bucket.access_logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id
  rule {
    id     = "expire-access-logs"
    status = "Enabled"
    filter {}
    expiration {
      days = var.access_logs_retention_days
    }
  }
}

resource "aws_s3_bucket_policy" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowLogDelivery"
      Effect    = "Allow"
      Principal = { Service = "logging.s3.amazonaws.com" }
      Action    = "s3:PutObject"
      Resource  = "${aws_s3_bucket.access_logs.arn}/*"
      Condition = {
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
      }
    }]
  })
}

################################################################################
# Invocation logging — S3 + CloudWatch
################################################################################

resource "aws_s3_bucket" "invocations" {
  bucket = "${local.prefix}-invocations-${data.aws_caller_identity.current.account_id}"
  # Object Lock requires bucket-level enablement at create time. Without
  # this flag the aws_s3_bucket_object_lock_configuration apply fails.
  object_lock_enabled = true
  tags                = local.tags
}

resource "aws_s3_bucket_logging" "invocations" {
  bucket        = aws_s3_bucket.invocations.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "invocations/"
}

resource "aws_s3_bucket_versioning" "invocations" {
  bucket = aws_s3_bucket.invocations.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "invocations" {
  bucket = aws_s3_bucket.invocations.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.logs_kms_key_arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "invocations" {
  bucket                  = aws_s3_bucket.invocations.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_object_lock_configuration" "invocations" {
  bucket = aws_s3_bucket.invocations.id
  rule {
    default_retention {
      mode = var.object_lock_mode
      days = var.object_lock_retention_days
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "invocations" {
  bucket = aws_s3_bucket.invocations.id

  rule {
    id     = "transition-to-ia-then-glacier"
    status = "Enabled"
    filter {}

    transition {
      days          = 90
      storage_class = "STANDARD_IA"
    }

    transition {
      days          = 365
      storage_class = "GLACIER"
    }
  }
}

# Bedrock's PutModelInvocationLoggingConfiguration validates that the
# target bucket has a policy authorizing bedrock.amazonaws.com to write.
# Without this policy the API call fails with
# "ValidationException: Failed to validate permissions for bucket".
# The aws:SourceAccount + aws:SourceArn conditions scope the trust to
# Bedrock acting on behalf of this account/log-config only.
resource "aws_s3_bucket_policy" "invocations" {
  bucket = aws_s3_bucket.invocations.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AllowBedrockWriteInvocationLogs"
        Effect    = "Allow"
        Principal = { Service = "bedrock.amazonaws.com" }
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.invocations.arn}/*"
        Condition = {
          StringEquals = {
            "aws:SourceAccount" = data.aws_caller_identity.current.account_id
          }
          ArnLike = {
            "aws:SourceArn" = "arn:aws:bedrock:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:*"
          }
        }
      }
    ]
  })
}

resource "aws_cloudwatch_log_group" "invocations" {
  name              = "/aws/bedrock/${local.prefix}/invocations"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.logs_kms_key_arn
  tags              = local.tags
}

resource "aws_iam_role" "bedrock_logging" {
  name = "${local.prefix}-logging"
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
      }
    }]
  })
}

resource "aws_iam_role_policy" "bedrock_logging" {
  name = "bedrock-logging"
  role = aws_iam_role.bedrock_logging.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "s3:PutObject",
          "s3:GetBucketLocation"
        ]
        Resource = [
          aws_s3_bucket.invocations.arn,
          "${aws_s3_bucket.invocations.arn}/*"
        ]
      },
      {
        Effect = "Allow"
        Action = [
          "logs:CreateLogStream",
          "logs:PutLogEvents"
        ]
        Resource = "${aws_cloudwatch_log_group.invocations.arn}:*"
      },
      {
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = var.logs_kms_key_arn
      }
    ]
  })
}

resource "aws_bedrock_model_invocation_logging_configuration" "this" {
  # Bedrock validates the bucket policy synchronously, so the policy must
  # be in place before this resource is created. Without the explicit
  # depends_on tofu races the two and validation can fail.
  depends_on = [aws_s3_bucket_policy.invocations]

  logging_config {
    embedding_data_delivery_enabled = true
    image_data_delivery_enabled     = true
    text_data_delivery_enabled      = true
    video_data_delivery_enabled     = true

    cloudwatch_config {
      log_group_name = aws_cloudwatch_log_group.invocations.name
      role_arn       = aws_iam_role.bedrock_logging.arn
    }

    s3_config {
      bucket_name = aws_s3_bucket.invocations.id
      key_prefix  = "invocations/"
    }
  }
}

################################################################################
# Baseline Guardrail
#
# Tenants override or extend via GuardrailPolicy CRs reconciled by the operator.
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
  # supports Bedrock Guardrails. SSM output and consumers (operator,
  # GuardrailPolicy CRs) handle baseline_guardrail_id being null gracefully.
  enable_guardrail = var.enable_guardrails_baseline && contains(local.guardrail_supported_regions, data.aws_region.current.region)
}

resource "aws_bedrock_guardrail" "baseline" {
  count = local.enable_guardrail ? 1 : 0

  name                      = "${local.prefix}-baseline"
  description               = "Platform baseline — denied topics + PII redaction. Tenants extend via GuardrailPolicy CRs."
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
# SSM outputs
################################################################################

resource "aws_ssm_parameter" "invocation_bucket" {
  name  = "/eks-agent-platform/${var.environment}/bedrock/invocation_bucket_arn"
  type  = "String"
  value = aws_s3_bucket.invocations.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "invocation_log_group" {
  name  = "/eks-agent-platform/${var.environment}/bedrock/invocation_log_group"
  type  = "String"
  value = aws_cloudwatch_log_group.invocations.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "baseline_guardrail_id" {
  count = local.enable_guardrail ? 1 : 0
  name  = "/eks-agent-platform/${var.environment}/bedrock/baseline_guardrail_id"
  type  = "String"
  value = aws_bedrock_guardrail.baseline[0].guardrail_id
  tags  = local.tags
}
