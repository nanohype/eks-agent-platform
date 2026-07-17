data "aws_caller_identity" "current" {}

locals {
  prefix = "${var.cluster_name}-artifacts"
  tags = merge(var.tags, {
    Component = "model-artifacts"
    Tier      = "platform"
  })
}

################################################################################
# Access-logs bucket — receives S3 server-access logs from artifacts + eval
# reports buckets. Separated from the data buckets so audit access can be
# scoped tightly (logs roles read here, never the data buckets).
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
# Model artifact bucket (LoRA / adapter / fine-tuned weights)
#
# Layout convention enforced via bucket policy:
#   s3://<bucket>/tenants/<platform-id>/<artifact-kind>/<artifact-id>/...
################################################################################

resource "aws_s3_bucket" "artifacts" {
  bucket = "${local.prefix}-${data.aws_caller_identity.current.account_id}"
  tags   = local.tags
}

resource "aws_s3_bucket_logging" "artifacts" {
  bucket        = aws_s3_bucket.artifacts.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "artifacts/"
}

resource "aws_s3_bucket_versioning" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.data_kms_key_arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    id     = "expire-noncurrent"
    status = "Enabled"
    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.lifecycle_noncurrent_expiration_days
    }
  }

  rule {
    id     = "abort-multipart"
    status = "Enabled"
    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

resource "aws_s3_bucket_policy" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          aws_s3_bucket.artifacts.arn,
          "${aws_s3_bucket.artifacts.arn}/*"
        ]
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      },
      {
        Sid       = "DenyUnencryptedObjectUploads"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.artifacts.arn}/*"
        Condition = {
          StringNotEquals = {
            "s3:x-amz-server-side-encryption" = "aws:kms"
          }
        }
      }
    ]
  })
}

################################################################################
# Eval reports bucket
################################################################################

resource "aws_s3_bucket" "eval_reports" {
  bucket = "${local.prefix}-eval-reports-${data.aws_caller_identity.current.account_id}"
  tags   = local.tags
}

resource "aws_s3_bucket_logging" "eval_reports" {
  bucket        = aws_s3_bucket.eval_reports.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "eval-reports/"
}

resource "aws_s3_bucket_versioning" "eval_reports" {
  bucket = aws_s3_bucket.eval_reports.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "eval_reports" {
  bucket = aws_s3_bucket.eval_reports.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.data_kms_key_arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "eval_reports" {
  bucket                  = aws_s3_bucket.eval_reports.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

################################################################################
# SSM outputs
################################################################################

resource "aws_ssm_parameter" "artifacts_bucket" {
  name  = "/eks-agent-platform/${var.cluster_name}/model-artifacts/bucket_arn"
  type  = "String"
  value = aws_s3_bucket.artifacts.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "eval_reports_bucket" {
  name  = "/eks-agent-platform/${var.cluster_name}/model-artifacts/eval_reports_bucket_arn"
  type  = "String"
  value = aws_s3_bucket.eval_reports.arn
  tags  = local.tags
}
