data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

# The operator role lives in landing-zone's canonical agent-iam component. Read
# its ARN and name from the SSM contract that component publishes, rather than
# carrying a duplicate agent-iam in this tree.
data "aws_ssm_parameter" "operator_role_arn" {
  name = "/eks-agent-platform/${var.cluster_name}/agent-iam/operator_role_arn"
}

data "aws_ssm_parameter" "operator_role_name" {
  name = "/eks-agent-platform/${var.cluster_name}/agent-iam/operator_role_name"
}

# The IAM path every role the operator mints lives under — tenant roles and
# attribution session roles alike. Read from the same agent-iam contract rather
# than restated here, so the cost publisher's tag-read grant is scoped by the
# component that owns the path.
data "aws_ssm_parameter" "tenant_iam_path" {
  name = "/eks-agent-platform/${var.cluster_name}/agent-iam/tenant_iam_path"
}

locals {
  prefix = "${var.cluster_name}-cost"
  tags = merge(var.tags, {
    Component = "cost-pipeline"
    Tier      = "platform"
  })

  # Teardown posture: development always permits a full destroy; elsewhere it is opt-in.
  # Same two-act contract as landing-zone's agent-iam — force_destroy has no effect until an
  # apply lands it in state, so permitting a teardown and performing one are separate acts.
  #
  # All three buckets need it. access-logs takes writes from the first PUT; cur and athena are
  # versioned, so their lifecycle rules write delete markers that are themselves current
  # versions and an expiry alone never empties them.
  bucket_force_destroy = var.environment == "development" || var.force_destroy_buckets

  # Where AWS delivers the CUR Parquet. The report definition and the lifecycle rule that ages
  # it out have to agree, so they read one value.
  cur_prefix = "cur"

  # The IAM path prefix for the cost publisher's tag-read grant, normalized the same way the
  # operator normalizes it before creating a role (platform_iam.go and platform_session_iam.go
  # both append a missing trailing slash). The path is used as a PREFIX on both sides, so
  # without the same normalization here a value of "/eks-agent-platform/tenants" yields the
  # grant `role/eks-agent-platform/tenants*` while the operator creates roles under
  # `/eks-agent-platform/tenants/` — a grant that matches nothing, every lookup AccessDenied,
  # every invocation attributed to "unknown", every budget reading low. One value read two ways
  # is the shape this whole component is being corrected for; it should not be reintroduced by
  # the fix.
  #
  # nonsensitive() because the aws_ssm_parameter data source marks every value
  # sensitive regardless of type, and that mark propagates through jsonencode into
  # the whole IAM policy document — so `tofu plan` would print
  # `policy = (sensitive value)` and hide every future change to the cost
  # publisher's permissions from review. An IAM path is a public-by-construction
  # string that this repo already writes out in full elsewhere; a plan that cannot
  # show a permissions diff is a worse outcome than naming it.
  tenant_iam_path_raw = nonsensitive(data.aws_ssm_parameter.tenant_iam_path.value)
  tenant_iam_path     = endswith(local.tenant_iam_path_raw, "/") ? local.tenant_iam_path_raw : "${local.tenant_iam_path_raw}/"

  # The Athena column AWS produces for the PlatformId cost-allocation tag. Every CUR consumer
  # in this component filters on it, and the operator's BudgetReconciler derives the same name
  # from the same tag key in Go (curTagColumn).
  #
  # Spelled out here rather than shared with the operator, because the two sides must agree by
  # both agreeing with AWS — one shared derivation would let a single wrong transform satisfy
  # both. AWS's rule, from "Column names" in the CUR user guide: an underscore is added in
  # front of uppercase letters, uppercase becomes lowercase, non-alphanumerics become
  # underscores, duplicates are removed. So `resourceTags/user:PlatformId` becomes the below —
  # note the split inside PlatformId, which is the part a hand-written name gets wrong.
  cur_platform_tag_column = "resource_tags_user_platform_id"
}

################################################################################
# Access-logs bucket — receives server-access logs from the CUR + Athena
# results buckets so audit access stays separable from the data path.
################################################################################

resource "aws_s3_bucket" "access_logs" {
  bucket = "${local.prefix}-access-logs-${data.aws_caller_identity.current.account_id}"

  # Takes writes from the first PUT the CUR and Athena buckets make.
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

  # Versioned, so an expiry alone cannot empty it — delete markers are current versions.
  force_destroy = local.bucket_force_destroy

  tags = local.tags
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

# The bucket default is SSE-S3 because the Cost and Usage Reports service cannot
# deliver into an SSE-KMS bucket. Its delivery role is the service principal
# billingreports.amazonaws.com holding exactly the two statements AWS publishes —
# GetBucketAcl/GetBucketPolicy and PutObject — and AWS documents no KMS key-policy
# statement for it anywhere, stating instead that exports are encrypted with
# SSE-S3 and that using SSE-KMS means re-encrypting after delivery. A CMK default
# here does not harden the bucket: it makes every PutObject fail, so the bucket
# stays empty and every budget decision reads zero.
#
# The estimates the cost publisher writes under estimates/ are NOT affected — the
# Lambda sets ServerSideEncryption=aws:kms and SSEKMSKeyId per object, which
# overrides the bucket default, so first-party data keeps the CMK and only the
# AWS-delivered billing detail is SSE-S3.
resource "aws_s3_bucket_server_side_encryption_configuration" "cur" {
  bucket = aws_s3_bucket.cur.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
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
        Principal = { AWS = data.aws_ssm_parameter.operator_role_arn.value }
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

# Estimate export retention — the invocation-cost-publisher writes small NDJSON
# objects under estimates/ on every log batch; bound their accumulation so the
# Athena scan stays cheap. The CUR Parquet under cur/ is untouched (the rule is
# prefix-scoped to estimates/).
resource "aws_s3_bucket_lifecycle_configuration" "cur" {
  bucket = aws_s3_bucket.cur.id

  rule {
    id     = "expire-estimates"
    status = "Enabled"
    filter {
      prefix = "${local.estimate_prefix}/"
    }
    expiration {
      days = var.estimate_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = 1
    }
  }

  # The CUR Parquet AWS delivers under cur/ is the bulk of this bucket and the input to every
  # budget decision, so it is kept for a full billing history and then aged out — the bucket has
  # a bounded size and a teardown has an end. Retention is measured from each object's last
  # delivery: report_versioning is OVERWRITE_REPORT and refresh_closed_reports is on, so AWS
  # rewrites a month's objects while it is still inside its refresh window and the clock
  # restarts. Past that window the objects are final and this rule is what bounds them.
  rule {
    id     = "expire-cur-parquet"
    status = "Enabled"
    filter {
      prefix = "${local.cur_prefix}/"
    }
    expiration {
      days = var.cur_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = 1
    }
  }

  rule {
    id     = "drop-expired-delete-markers"
    status = "Enabled"
    filter {}

    # Same bookkeeping problem as athena-results: on a versioned bucket an expiry writes a
    # delete marker rather than removing the object, and the marker is a current version.
    expiration {
      expired_object_delete_marker = true
    }
  }
}

resource "aws_cur_report_definition" "this" {
  # us-east-1 only — the CUR API has no endpoint anywhere else, so this resource
  # cannot be created through the workload-region provider at all.
  provider = aws.us_east_1

  report_name                = var.cur_report_name
  time_unit                  = "HOURLY"
  format                     = "Parquet"
  compression                = "Parquet"
  additional_schema_elements = ["RESOURCES", "SPLIT_COST_ALLOCATION_DATA"]
  s3_bucket                  = aws_s3_bucket.cur.id
  # AWS validation requires this NOT end with `/` or `.` — `^.+[^/|.]$`.
  s3_prefix              = local.cur_prefix
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

  # Versioned, and every reconciler tick writes a result object here.
  force_destroy = local.bucket_force_destroy

  tags = local.tags
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

    # The bucket is versioned, so the expiry above does not delete anything — it writes a
    # delete marker, which is itself the object's new current version. Without these two the
    # bucket only ever grows, and both the storage bill and a teardown outlive the retention
    # the rule appears to set.
    noncurrent_version_expiration {
      noncurrent_days = 1
    }
  }

  rule {
    id     = "drop-expired-delete-markers"
    status = "Enabled"
    filter {}

    # A delete marker with no versions left under it is pure bookkeeping that still counts as
    # an object. S3 rejects days/date in the same expiration block as this flag, so it is its
    # own rule.
    expiration {
      expired_object_delete_marker = true
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
        Sid    = "CostDataKMS"
        Effect = "Allow"
        # Athena runs S3 and KMS access under the CALLER's identity, and the caller
        # here is the operator role this policy attaches to. Three actions, all
        # load-bearing on the same key:
        #   Decrypt         - read the estimates/ objects the cost publisher writes
        #                     with an explicit SSE-KMS header, and read a result set
        #                     back on GetQueryResults
        #   GenerateDataKey - WRITE the result set. The workgroup sets
        #                     enforce_workgroup_configuration with SSE_KMS results,
        #                     so this is not optional, and Decrypt alone leaves every
        #                     query failing at the write step
        #   DescribeKey     - the SDK's key-resolution path
        #
        # Without these the query returns FAILED on every tick, reconcileBudget
        # returns before the kill-switch block, and a tenant at 120% of budget never
        # trips it with nothing anywhere going red.
        Action   = ["kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
        Resource = [var.data_kms_key_arn]
        Condition = {
          StringEquals = {
            "kms:ViaService" = ["s3.${var.region}.amazonaws.com"]
          }
        }
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
  role       = data.aws_ssm_parameter.operator_role_name.value
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
    path = "s3://${aws_s3_bucket.cur.id}/${local.cur_prefix}/${var.cur_report_name}/"
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
  cur_table_name      = replace(var.cur_report_name, "-", "_")
  estimate_prefix     = "estimates"
  estimate_table_name = "invocation_cost_estimates"
  reconciliation_view = "invocation_cost_reconciliation"
}

################################################################################
# Estimate export + reconciliation
#
# The invocation-cost-publisher Lambda (below) also writes per-(platform, model)
# daily estimate records as Hive-partitioned NDJSON under <cur-bucket>/estimates/
# usage_date=<d>/. This Glue table reads that prefix via partition projection
# (date on usage_date; platform_id is a data column, not a partition, so the
# reconciliation view can aggregate across all platforms without a per-partition
# predicate). The reconciliation view LEFT JOINs the daily estimate against the
# CUR truth so finance can watch estimate-vs-billed drift.
################################################################################

resource "aws_glue_catalog_table" "estimates" {
  database_name = aws_glue_catalog_database.cost.name
  name          = local.estimate_table_name
  table_type    = "EXTERNAL_TABLE"

  parameters = {
    classification                        = "json"
    "projection.enabled"                  = "true"
    "projection.usage_date.type"          = "date"
    "projection.usage_date.format"        = "yyyy-MM-dd"
    "projection.usage_date.range"         = "2025-01-01,NOW"
    "projection.usage_date.interval"      = "1"
    "projection.usage_date.interval.unit" = "DAYS"
    "storage.location.template"           = "s3://${aws_s3_bucket.cur.id}/${local.estimate_prefix}/usage_date=$${usage_date}"
  }

  storage_descriptor {
    location      = "s3://${aws_s3_bucket.cur.id}/${local.estimate_prefix}/"
    input_format  = "org.apache.hadoop.mapred.TextInputFormat"
    output_format = "org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat"

    ser_de_info {
      serialization_library = "org.openx.data.jsonserde.JsonSerDe"
    }

    columns {
      name = "platform_id"
      type = "string"
    }
    columns {
      name = "model_id"
      type = "string"
    }
    columns {
      name = "estimate_usd"
      type = "double"
    }
    columns {
      name = "input_tokens"
      type = "bigint"
    }
    columns {
      name = "output_tokens"
      type = "bigint"
    }
    columns {
      name = "invocation_count"
      type = "bigint"
    }
  }

  partition_keys {
    name = "usage_date"
    type = "string"
  }
}

# Reconciliation view definition, version-controlled as a saved query. Athena
# Glue VIRTUAL_VIEWs require a base64 Presto envelope whose column types must
# match exactly; a saved `CREATE OR REPLACE VIEW` is plain, reviewable SQL that
# the operator (or an analyst) runs once to materialize the view in the cost
# database. The finance dashboard reads the materialized view by name.
#
# ── Two Bedrock product codes, not one ─────────────────────────────────────
#
# Anthropic models do not bill as `AmazonBedrock`. They are sold through AWS
# Marketplace, so each model is its own billing product with its own opaque
# `line_item_product_code` — measured in a live account, `35bl0uzthq3u3dp0hocpb4n84`
# is Claude Sonnet 5, and its monthly unblended cost matches Cost Explorer's
# "Claude Sonnet 5 (Amazon Bedrock Edition)" to the cent. Those codes are per
# product and are not stable identifiers to enumerate, so this filters on the
# product *name*, which is the same string Cost Explorer shows as the service and
# the same string the Price List returns in `servicename`.
#
# Filtering on `AmazonBedrock` alone captured $0.01 of $0.78 of Claude spend in
# the month this was measured — Amazon's own models (Nova, Titan) and the Bedrock
# service charges, and none of Anthropic's.
#
# ── This view is inert until per-tenant tags reach CUR ─────────────────────
#
# The join key is a cost allocation tag, and no Bedrock line item in the measured
# account carried one — neither the marketplace rows nor the `AmazonBedrock` rows.
# Two things have to be true before it can, and neither is yet:
#
#  1. An InvokeModel call has no taggable resource of its own. Attaching a tag to
#     `bedrock-runtime` spend requires invoking through a per-tenant *application
#     inference profile* whose tags flow to CUR, in place of the raw model or
#     cross-region profile ID.
#  2. `platformid` must be activated as a cost allocation tag in Billing.
#     Activation is not retroactive, so nothing before it is ever attributed.
#
# Until both land, `cur_truth_usd` is NULL for every row. That is reported as
# `match_state = 'no_cur_row'` rather than as a NULL delta, because a
# reconciliation that returns nothing and a reconciliation that finds no
# disagreement render identically on a dashboard, and the first one was being read
# as the second.
resource "aws_athena_named_query" "spend_reconciliation" {
  name        = "${local.prefix}-reconciliation"
  workgroup   = aws_athena_workgroup.cost.id
  database    = aws_glue_catalog_database.cost.name
  description = "Create/refresh the invocation_cost_reconciliation view (daily estimate vs CUR truth, per platform)."

  query = <<-SQL
    CREATE OR REPLACE VIEW ${local.reconciliation_view} AS
    SELECT
      e.platform_id,
      e.usage_date                        AS day,
      e.estimate_usd                      AS estimate_usd,
      c.cur_truth_usd                     AS cur_truth_usd,
      (e.estimate_usd - c.cur_truth_usd)  AS delta_usd,
      CASE WHEN c.cur_truth_usd IS NULL THEN 'no_cur_row'
           WHEN c.cur_truth_usd = 0      THEN 'cur_row_zero_cost'
           ELSE 'compared'
      END                                 AS match_state,
      CASE WHEN c.cur_truth_usd > 0
           THEN abs(e.estimate_usd - c.cur_truth_usd) / c.cur_truth_usd
           ELSE NULL
      END                                 AS delta_pct
    FROM (
      SELECT platform_id, usage_date, SUM(estimate_usd) AS estimate_usd
      FROM ${local.estimate_table_name}
      GROUP BY platform_id, usage_date
    ) e
    LEFT JOIN (
      SELECT ${local.cur_platform_tag_column}      AS platform_id,
             date_format(line_item_usage_start_date, '%Y-%m-%d') AS day,
             SUM(line_item_unblended_cost)                       AS cur_truth_usd
      FROM ${local.cur_table_name}
      WHERE (line_item_product_code = 'AmazonBedrock'
             OR product_product_name LIKE '%(Amazon Bedrock Edition)%')
        AND line_item_line_item_type = 'Usage'
        AND ${local.cur_platform_tag_column} <> ''
      GROUP BY ${local.cur_platform_tag_column},
               date_format(line_item_usage_start_date, '%Y-%m-%d')
    ) c ON e.platform_id = c.platform_id AND e.usage_date = c.day
  SQL
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

# Package the whole lambda/ directory so the generated pricing_data.py ships
# alongside the handler. Tests and bytecode caches are excluded — they never
# run in Lambda.
data "archive_file" "invocation_cost_publisher" {
  type        = "zip"
  source_dir  = "${path.module}/lambda"
  output_path = "${path.module}/build/invocation_cost_publisher.zip"
  excludes    = ["test_invocation_cost_publisher.py", "__pycache__", "requirements-test.txt"]
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
      },
      {
        Sid    = "ReadPlatformIdTag"
        Effect = "Allow"
        # The publisher reads the PlatformId tag off the role that made each
        # invocation, rather than reconstructing it from the role's NAME. That is
        # the whole correctness argument for the attribution path — the writer
        # (the operator) and this reader cannot disagree about a value only one
        # of them computes — so this grant is load-bearing, not incidental.
        # Without it every lookup raises, every invocation attributes to
        # "unknown", and every tenant's in-flight spend reads zero.
        #
        # Scoped to the operator's IAM path, which is where every role it mints
        # lives. ListRoleTags is read-only and returns tags for one named role.
        Action   = ["iam:ListRoleTags"]
        Resource = ["arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role${local.tenant_iam_path}*"]
      },
      {
        Sid      = "WriteEstimates"
        Effect   = "Allow"
        Action   = ["s3:PutObject"]
        Resource = ["${aws_s3_bucket.cur.arn}/${local.estimate_prefix}/*"]
      },
      {
        Sid      = "EncryptEstimates"
        Effect   = "Allow"
        Action   = ["kms:GenerateDataKey", "kms:Encrypt", "kms:DescribeKey"]
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

  environment {
    variables = {
      # No environment or cluster token is passed, on purpose. The publisher does
      # not derive the PlatformId dimension from anything it is configured with —
      # it reads the tag the operator stamped on the invoking role. A token here
      # would be a second place the identity is decided, which is precisely the
      # arrangement that made the dimension disagree with every reader.
      ESTIMATE_BUCKET     = aws_s3_bucket.cur.id
      ESTIMATE_PREFIX     = local.estimate_prefix
      ESTIMATE_KMS_KEY_ID = var.data_kms_key_arn
      # Per-token governance estimate for imported (Custom Model Import) models,
      # which are capacity-billed and carry no pricing-table row. 0 leaves them
      # unpriced-but-observable (UnpricedInvocations); a positive value brings
      # imported spend into the kill-switch cost signal. See the Lambda header.
      IMPORTED_MODEL_ESTIMATE_USD_PER_MTOKENS = tostring(var.imported_model_estimate_usd_per_mtokens)
    }
  }

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
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/cur_bucket"
  type  = "String"
  value = aws_s3_bucket.cur.id
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_workgroup" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_workgroup"
  type  = "String"
  value = aws_athena_workgroup.cost.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_database" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_database"
  type  = "String"
  value = aws_glue_catalog_database.cost.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "cur_table_name" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/cur_table_name"
  type  = "String"
  value = local.cur_table_name
  tags  = local.tags
}

resource "aws_ssm_parameter" "estimate_table_name" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/estimate_table_name"
  type  = "String"
  value = local.estimate_table_name
  tags  = local.tags
}

resource "aws_ssm_parameter" "reconciliation_view" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/reconciliation_view"
  type  = "String"
  value = local.reconciliation_view
  tags  = local.tags
}
