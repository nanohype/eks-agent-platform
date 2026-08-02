data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  # Account + region, and nothing else. No cluster token and no environment token:
  # this component is the single owner of resources there is exactly one of per
  # account per region, so a name that carried either would be a claim it cannot
  # keep. `org-` matches the org's account-scoped bucket idiom (landing-zone's
  # org-cost uses `org-<account>-cur-export`); the region is load-bearing because
  # S3's namespace is global while Bedrock logging is per-region.
  prefix = "${var.environment}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.region}-bedrock"

  tags = merge(var.tags, {
    Component = "bedrock-account"
    Tier      = "platform"
    Scope     = "account"
  })

  # The invocation record is the account's audit of every model call, and this
  # component is applied once rather than per environment, so there is no
  # environment that could make it disposable. The teardown lever the
  # per-environment components carry deliberately does not exist here — see the
  # object_lock_mode contract for what does govern deletion.
  bucket_force_destroy = var.object_lock_mode != "COMPLIANCE"
}

################################################################################
# Bedrock model-invocation logging — ACCOUNT-SCOPED, one instance per region.
#
# `aws_bedrock_model_invocation_logging_configuration` has no name and no
# identifier. There is exactly one per account per region, so it cannot be owned by
# a per-environment component: a second environment applying it silently repoints
# the account's logging at that environment's bucket, and ANY environment's teardown
# deletes it for all of them. Both applies are green and nothing goes red — the
# invocation record is the signal every budget decision reads, so it is the org's
# own failure class sitting on the flagship feature.
#
# So this component exists to be the single owner. It has one live root
# (`live/org/bedrock-account`), it takes no cluster_name, and nothing here is named
# for an environment.
#
# What it owns, and why each piece has to be here rather than next door:
#
#   invocations bucket   the logging destination. Named for the account and region
#                        because S3 is a global namespace and Bedrock logging is
#                        per-region, so two regions in one account must not collide.
#   access-logs bucket   its only writer is the invocations bucket. Leaving it in the
#                        per-environment component would strand a bucket with no
#                        producer, or force the account-scoped log to write into one
#                        environment's sink — the same defect one layer down.
#   log group + role     the CloudWatch half of the same single configuration.
#
# What it deliberately does NOT own:
#
#   the cost publisher   it subscribes to this log group, but it writes its estimates
#                        into the CUR bucket and its table lives in the cost database,
#                        so it belongs to cost-pipeline — which is account-scoped for
#                        its own reason (a CUR has no filter and always covers the
#                        whole account). cost-pipeline reads the log-group name from
#                        this component's SSM contract.
#
#                        Worth stating because the pairing is not optional: a log
#                        group accepts five subscription filters, so three
#                        per-environment publishers WOULD all attach cleanly — and
#                        each would process every record into the account-global
#                        agents/Bedrock namespace, which the budget reconciler reads
#                        with Stat=Sum. Three times the real number, with three green
#                        applies. An apply that failed would have been kinder.
#
#   the PlatformId tag   activating a cost-allocation tag is account-global, but it
#                        is part of the cost pipeline and it cannot happen in the same
#                        apply that first stamps the key (AWS takes up to 24h to LIST
#                        a newly observed one). Holding it here would gate the account's
#                        only Bedrock audit trail behind a billing clock. It lives in
#                        cost-pipeline, count-gated on observation.
################################################################################

################################################################################
# Access-logs bucket — receives server-access logs from the invocations
# bucket. Kept separate so audit access can be scoped tightly.
################################################################################

resource "aws_s3_bucket" "access_logs" {
  bucket = "${local.prefix}-access-logs"

  # Server-access logs land from the first PUT the invocations bucket takes, so this is
  # non-empty long before anyone tries to tear the environment down.
  force_destroy = local.bucket_force_destroy

  tags = local.tags
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
  bucket = "${local.prefix}-invocations"
  # Object Lock is enabled at create time (immutable afterward) so the bucket
  # can enforce WORM retention on invocation logs. force_destroy is permitted
  # under GOVERNANCE — where an admin holding s3:BypassGovernanceRetention can
  # clear the lock — so environments tear down cleanly. Under COMPLIANCE the
  # objects (and therefore the bucket) cannot be deleted by anyone until
  # retention expires, so destroy is intentionally left blocked.
  object_lock_enabled = true
  force_destroy       = var.object_lock_mode != "COMPLIANCE"
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

    # Object Lock holds every version until its own retain-until date, so nothing here can
    # delete a locked object early — this is what bounds the bucket once the lock lapses.
    # Without it the invocation record grows for the life of the account: transitions move
    # the cost down a tier and never end it, and a teardown has nothing to reach.
    expiration {
      days = var.object_lock_retention_days + 1
    }

    noncurrent_version_expiration {
      noncurrent_days = 1
    }
  }

  rule {
    id     = "drop-expired-delete-markers"
    status = "Enabled"
    filter {}

    # A delete marker with no versions left under it is bookkeeping that still counts as an
    # object. S3 rejects days/date in the same expiration block as this flag, so it is its
    # own rule.
    expiration {
      expired_object_delete_marker = true
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
