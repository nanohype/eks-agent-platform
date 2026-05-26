data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  prefix = "${var.environment}-${var.cluster_name}-cost"
  tags = merge(var.tags, {
    Component = "cost-pipeline"
    Tier      = "platform"
  })
}

################################################################################
# Access-logs bucket — receives server-access logs from the CUR + Athena
# results buckets so audit access stays separable from the data path.
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
# CUR report bucket
#
# Cost & Usage Reports v1 API (aws_cur_report_definition + the
# billingreports.amazonaws.com service principal). The CUR resource itself
# must be created in us-east-1; the destination S3 bucket can live in any
# region. Consumers pass a region-aliased provider for the report
# definition; the bucket is created in the workload region.
#
# Migration path: aws_bcmdataexports_export + bcm-data-exports.amazonaws.com
# is the successor API; the CUR v1 surface remains supported by AWS for
# existing definitions until further notice.
################################################################################

resource "aws_s3_bucket" "cur" {
  bucket = "${local.prefix}-cur-${data.aws_caller_identity.current.account_id}"
  tags   = local.tags
}

resource "aws_s3_bucket_logging" "cur" {
  bucket        = aws_s3_bucket.cur.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "cur/"
}

resource "aws_s3_bucket_versioning" "cur" {
  bucket = aws_s3_bucket.cur.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "cur" {
  bucket = aws_s3_bucket.cur.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.data_kms_key_arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "cur" {
  bucket                  = aws_s3_bucket.cur.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "cur" {
  bucket = aws_s3_bucket.cur.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "BillingReportsAccess"
        Effect = "Allow"
        Principal = {
          Service = "billingreports.amazonaws.com"
        }
        Action = [
          "s3:GetBucketAcl",
          "s3:GetBucketPolicy",
          "s3:PutObject"
        ]
        Resource = [
          aws_s3_bucket.cur.arn,
          "${aws_s3_bucket.cur.arn}/*"
        ]
        Condition = {
          StringEquals = {
            "aws:SourceAccount" = data.aws_caller_identity.current.account_id
          }
          # The CUR (Reports v1) API is global with ARN region always
          # 'us-east-1' regardless of where the destination bucket
          # lives. Don't substitute var.region here or billingreports
          # PutObject silently fails when the workload region differs
          # — the bucket stays empty and the Budget reconciler reports
          # zero spend forever.
          ArnLike = {
            "aws:SourceArn" = "arn:${data.aws_partition.current.partition}:cur:us-east-1:${data.aws_caller_identity.current.account_id}:definition/*"
          }
        }
      },
      {
        Sid       = "OperatorRead"
        Effect    = "Allow"
        Principal = { AWS = var.operator_role_arn }
        Action = [
          "s3:GetObject",
          "s3:ListBucket"
        ]
        Resource = [
          aws_s3_bucket.cur.arn,
          "${aws_s3_bucket.cur.arn}/*"
        ]
      }
    ]
  })
}

resource "aws_cur_report_definition" "this" {
  report_name                = var.cur_report_name
  time_unit                  = "HOURLY"
  format                     = "Parquet"
  compression                = "Parquet"
  additional_schema_elements = ["RESOURCES", "SPLIT_COST_ALLOCATION_DATA"]
  s3_bucket                  = aws_s3_bucket.cur.id
  # AWS validation requires this NOT end with `/` or `.` — `^.+[^/|.]$`.
  s3_prefix              = "cur"
  s3_region              = var.region
  additional_artifacts   = ["ATHENA"]
  refresh_closed_reports = true
  report_versioning      = "OVERWRITE_REPORT"

  depends_on = [aws_s3_bucket_policy.cur]
}

################################################################################
# Athena workgroup + database
################################################################################

resource "aws_s3_bucket" "athena_results" {
  bucket = "${local.prefix}-athena-${data.aws_caller_identity.current.account_id}"
  tags   = local.tags
}

resource "aws_s3_bucket_logging" "athena_results" {
  bucket        = aws_s3_bucket.athena_results.id
  target_bucket = aws_s3_bucket.access_logs.id
  target_prefix = "athena-results/"
}

resource "aws_s3_bucket_versioning" "athena_results" {
  bucket = aws_s3_bucket.athena_results.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "athena_results" {
  bucket = aws_s3_bucket.athena_results.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.data_kms_key_arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "athena_results" {
  bucket                  = aws_s3_bucket.athena_results.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "athena_results" {
  bucket = aws_s3_bucket.athena_results.id

  rule {
    id     = "expire-results"
    status = "Enabled"
    filter {}

    expiration {
      days = var.athena_results_retention_days
    }
  }
}

resource "aws_athena_workgroup" "cost" {
  name = local.prefix
  tags = local.tags

  configuration {
    enforce_workgroup_configuration    = true
    publish_cloudwatch_metrics_enabled = true

    result_configuration {
      output_location = "s3://${aws_s3_bucket.athena_results.id}/results/"

      encryption_configuration {
        encryption_option = "SSE_KMS"
        kms_key_arn       = var.data_kms_key_arn
      }
    }
  }
}

resource "aws_glue_catalog_database" "cost" {
  name = replace("${local.prefix}-cost", "-", "_")
  tags = local.tags
}

################################################################################
# Operator policy attachment — read CUR + run Athena queries
################################################################################

resource "aws_iam_policy" "operator_cost" {
  name = "${local.prefix}-operator-cost"
  path = "/eks-agent-platform/"
  tags = local.tags

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "AthenaQuery"
        Effect = "Allow"
        Action = [
          "athena:StartQueryExecution",
          "athena:GetQueryExecution",
          "athena:GetQueryResults",
          "athena:StopQueryExecution",
          "athena:GetWorkGroup"
        ]
        Resource = aws_athena_workgroup.cost.arn
      },
      {
        Sid    = "GlueRead"
        Effect = "Allow"
        Action = [
          "glue:GetDatabase",
          "glue:GetTable",
          "glue:GetTables",
          "glue:GetPartitions"
        ]
        Resource = [
          "arn:${data.aws_partition.current.partition}:glue:${var.region}:${data.aws_caller_identity.current.account_id}:catalog",
          aws_glue_catalog_database.cost.arn,
          "arn:${data.aws_partition.current.partition}:glue:${var.region}:${data.aws_caller_identity.current.account_id}:table/${aws_glue_catalog_database.cost.name}/*"
        ]
      },
      {
        Sid    = "AthenaResults"
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:ListBucket"
        ]
        Resource = [
          aws_s3_bucket.athena_results.arn,
          "${aws_s3_bucket.athena_results.arn}/*"
        ]
      },
      {
        Sid    = "BedrockMetrics"
        Effect = "Allow"
        Action = [
          "cloudwatch:GetMetricStatistics",
          "cloudwatch:GetMetricData",
          "cloudwatch:ListMetrics"
        ]
        Resource = "*"
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "operator_cost" {
  role       = var.operator_role_name
  policy_arn = aws_iam_policy.operator_cost.arn
}

################################################################################
# Glue Crawler — catalogs the CUR Parquet files into the Glue database so
# Athena can query them. Runs daily; the operator's Budget reconciler then
# issues an aggregating SUM(line_item_unblended_cost) query grouped by the
# PlatformId resource tag.
#
# The crawler picks up the partition columns (year, month) from the CUR
# directory layout automatically. The resulting Glue table is named after
# the CUR report name (with hyphens normalized to underscores by the
# Crawler's default schema-change-policy).
################################################################################

resource "aws_iam_role" "cur_crawler" {
  name = "${local.prefix}-crawler"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "glue.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "cur_crawler_glue_service" {
  role       = aws_iam_role.cur_crawler.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AWSGlueServiceRole"
}

resource "aws_iam_role_policy" "cur_crawler" {
  name = "cur-bucket-read"
  role = aws_iam_role.cur_crawler.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ReadCURBucket"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:ListBucket"]
        Resource = [aws_s3_bucket.cur.arn, "${aws_s3_bucket.cur.arn}/*"]
      },
      {
        Sid      = "DecryptCURObjects"
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:DescribeKey", "kms:GenerateDataKey"]
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

resource "aws_glue_crawler" "cur" {
  name          = "${local.prefix}-cur"
  database_name = aws_glue_catalog_database.cost.name
  role          = aws_iam_role.cur_crawler.arn
  schedule      = var.cur_crawler_schedule
  tags          = local.tags

  s3_target {
    path = "s3://${aws_s3_bucket.cur.id}/cur/${var.cur_report_name}/"
  }

  schema_change_policy {
    delete_behavior = "LOG"
    update_behavior = "UPDATE_IN_DATABASE"
  }

  recrawl_policy {
    # CUR is overwrite-style (report_versioning = OVERWRITE_REPORT); a full
    # recrawl on every run catches schema additions when AWS adds new
    # columns.
    recrawl_behavior = "CRAWL_EVERYTHING"
  }
}

# Predicted table name after Crawler runs. Glue normalizes hyphens in the
# CUR report name to underscores. Published to SSM so the operator can
# discover it without hard-coding.
locals {
  cur_table_name = replace(var.cur_report_name, "-", "_")
}

################################################################################
# Invocation-cost-publisher Lambda
#
# Subscribes to the Bedrock invocation log group emitted by
# terraform/components/bedrock and republishes the per-invocation cost
# as a CloudWatch custom metric dimensioned by PlatformId. The Budget
# reconciler reads this metric via GetMetricData to get sub-CUR-partition
# in-flight cost (Bedrock invocation logs land in seconds; CUR partitions
# lag by ~24h).
################################################################################

data "archive_file" "invocation_cost_publisher" {
  type        = "zip"
  source_file = "${path.module}/lambda/invocation_cost_publisher.py"
  output_path = "${path.module}/build/invocation_cost_publisher.zip"
}

resource "aws_iam_role" "invocation_cost_publisher" {
  name = "${local.prefix}-invcost"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_cloudwatch_log_group" "invocation_cost_publisher" {
  name              = "/aws/lambda/${local.prefix}-invcost"
  retention_in_days = var.invocation_cost_publisher_log_retention_days
  kms_key_id        = var.logs_kms_key_arn
  tags              = local.tags
}

resource "aws_iam_role_policy" "invocation_cost_publisher" {
  name = "publish-cost-metric"
  role = aws_iam_role.invocation_cost_publisher.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteOwnLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = ["${aws_cloudwatch_log_group.invocation_cost_publisher.arn}:*"]
      },
      {
        Sid      = "PublishMetric"
        Effect   = "Allow"
        Action   = ["cloudwatch:PutMetricData"]
        Resource = "*"
        Condition = {
          StringEquals = {
            "cloudwatch:namespace" = "agents/Bedrock"
          }
        }
      }
    ]
  })
}

resource "aws_lambda_function" "invocation_cost_publisher" {
  function_name    = "${local.prefix}-invcost"
  role             = aws_iam_role.invocation_cost_publisher.arn
  runtime          = "python3.12"
  handler          = "invocation_cost_publisher.handler"
  filename         = data.archive_file.invocation_cost_publisher.output_path
  source_code_hash = data.archive_file.invocation_cost_publisher.output_base64sha256
  memory_size      = 256
  timeout          = 30
  tags             = local.tags

  # Reserved concurrency — invocation logs can burst (Bedrock multi-region
  # inference profiles spawn parallel writes). Cap at a reasonable
  # parallelism so a runaway tenant can't drain the account's Lambda quota.
  reserved_concurrent_executions = 25

  tracing_config {
    mode = "Active"
  }

  depends_on = [aws_cloudwatch_log_group.invocation_cost_publisher]
}

resource "aws_lambda_permission" "invocation_cost_publisher_logs" {
  statement_id  = "AllowCloudWatchLogsInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.invocation_cost_publisher.function_name
  principal     = "logs.${var.region}.amazonaws.com"
  source_arn    = "arn:${data.aws_partition.current.partition}:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:${var.bedrock_invocation_log_group}:*"
}

resource "aws_cloudwatch_log_subscription_filter" "invocations" {
  name            = "${local.prefix}-invcost"
  log_group_name  = var.bedrock_invocation_log_group
  filter_pattern  = "" # match everything; the Lambda decides what counts
  destination_arn = aws_lambda_function.invocation_cost_publisher.arn
  distribution    = "ByLogStream"

  depends_on = [aws_lambda_permission.invocation_cost_publisher_logs]
}

################################################################################
# SSM outputs
################################################################################

resource "aws_ssm_parameter" "cur_bucket" {
  name  = "/eks-agent-platform/${var.environment}/cost-pipeline/cur_bucket"
  type  = "String"
  value = aws_s3_bucket.cur.id
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_workgroup" {
  name  = "/eks-agent-platform/${var.environment}/cost-pipeline/athena_workgroup"
  type  = "String"
  value = aws_athena_workgroup.cost.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_database" {
  name  = "/eks-agent-platform/${var.environment}/cost-pipeline/athena_database"
  type  = "String"
  value = aws_glue_catalog_database.cost.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "cur_table_name" {
  name  = "/eks-agent-platform/${var.environment}/cost-pipeline/cur_table_name"
  type  = "String"
  value = local.cur_table_name
  tags  = local.tags
}
