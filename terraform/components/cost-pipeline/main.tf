################################################################################
# The account's cost pipeline — ONE of it, not one per environment.
#
# `aws_cur_report_definition` has no filter. A Cost and Usage Report always covers
# the entire account, so three per-environment reports are not three views of
# anything: they are three complete, identical copies of the same billing data, in
# three buckets, crawled by three crawlers, into three Glue databases, queried
# through three Athena workgroups. Every copy is correct and every copy is the whole
# account, which is why nothing ever went red — the duplication is a bill and a
# maintenance surface rather than a broken query.
#
# The same is true one layer down. The Bedrock invocation log group is an
# account+region singleton (bedrock-account owns it), and a log group accepts five
# subscription filters — so three per-environment cost publishers WOULD all attach
# cleanly, and each would process every record into the account-global
# `agents/Bedrock` namespace that the budget reconciler reads with Stat=Sum. Three
# times the real number, three green applies. An apply that failed would have been
# kinder than one that succeeded.
#
# So this component is applied once, from live/org/, and everything per-cluster
# lives in cost-access: the operator's IAM grant, and the republished handles under
# the cluster prefix the operator actually sweeps.
################################################################################

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}
data "aws_region" "current" {}

locals {
  # Account + region. Region is load-bearing even though a CUR's DATA is
  # account-global: these are S3 buckets and a Glue database, which live in a region
  # and share a global namespace, so a second region must not collide on the name.
  prefix = "${var.environment}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.region}-cost"

  tags = merge(var.tags, {
    Component = "cost-pipeline"
    Tier      = "platform"
    Scope     = "account"

    # Stamped on this component's own resources to make the key OBSERVABLE, which is
    # what starts AWS's clock toward being able to activate it. The pipeline's own
    # storage genuinely belongs to the account rather than to any tenant, so `org` is
    # the honest value and not a placeholder.
    #
    # Without this nothing in a fresh account ever carries the key, AWS never lists
    # it, and the activation below can never happen — the tenants that would carry it
    # do not exist until the platform does.
    PlatformId = "org"
  })

  # Teardown posture. There is no environment token to branch on here — this
  # component is applied once for the account, so `development` is not a thing it
  # can be. The lever is the explicit one only.
  #
  # All three buckets need it. access-logs takes writes from the first PUT; cur and
  # athena are versioned, so their lifecycle rules write delete markers that are
  # themselves current versions and an expiry alone never empties them.
  bucket_force_destroy = var.force_destroy_buckets

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
  # It arrives as a variable rather than from agent-iam's SSM contract, and that is a
  # downgrade forced by the scope change: agent-iam publishes the path per CLUSTER,
  # and this component is applied once for the account, so there is no single cluster
  # subtree it could read. The value is an account-wide constant in landing-zone
  # (agent-iam's local.tenant_role_path), which is why one variable can stand for all
  # of them.
  #
  # It is not left as an unchecked mirror. This component publishes the path it used,
  # and every cluster's cost-access reads BOTH that and its own agent-iam parameter
  # and refuses to create the operator grant if they disagree — so the constant is
  # verified once per cluster, at the layer that can see both values, instead of
  # being asserted by a comment here.
  tenant_iam_path = endswith(var.tenant_iam_path, "/") ? var.tenant_iam_path : "${var.tenant_iam_path}/"

  # nonsensitive() because the aws_ssm_parameter data source marks every value
  # sensitive regardless of type, and that mark would propagate into the Lambda
  # permission's source_arn and the subscription filter — printing them as
  # (sensitive value) in every plan and hiding which log group the account's cost
  # publisher is about to attach itself to.
  bedrock_invocation_log_group = nonsensitive(data.aws_ssm_parameter.bedrock_invocation_log_group.value)

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
      }
      # No OperatorRead statement. It named ONE cluster's operator role, which a
      # bucket serving every cluster cannot do — a bucket policy is a single
      # document, so N clusters would mean N writers rewriting one object and the
      # last apply deciding who still has access.
      #
      # The operators get their read through their own IAM policies instead, minted
      # per cluster by cost-access. Within one account an identity policy is
      # sufficient on its own; a resource policy adds nothing here except a
      # contended object.
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
  name = replace(local.prefix, "-", "_")
  tags = local.tags
}

################################################################################
# The operator's grant is NOT here.
#
# It attaches to one cluster's operator role, discovered from that cluster's
# agent-iam SSM contract — a per-cluster act, and this component is applied once for
# the account. N clusters need N policies and N attachments, so they live in
# cost-access, which is applied per cluster and reads this component's published
# handles.
#
# What stays here is what there is one of: the report, the buckets, the catalog, the
# workgroup and the publisher.
################################################################################

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

# The Bedrock invocation log group is an account+region singleton owned by
# bedrock-account. Read from its published contract rather than taken as an input
# from a terragrunt `dependency`: a dependency across roots resolves at PARSE time,
# so this root could not even `init` before the account's logging existed.
data "aws_ssm_parameter" "bedrock_invocation_log_group" {
  name = "/eks-agent-platform/org/bedrock-account/invocation_log_group"
}

resource "aws_lambda_permission" "invocation_cost_publisher_logs" {
  statement_id  = "AllowCloudWatchLogsInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.invocation_cost_publisher.function_name
  principal     = "logs.${var.region}.amazonaws.com"
  source_arn    = "arn:${data.aws_partition.current.partition}:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:${local.bedrock_invocation_log_group}:*"
}

resource "aws_cloudwatch_log_subscription_filter" "invocations" {
  name            = "${local.prefix}-invcost"
  log_group_name  = local.bedrock_invocation_log_group
  filter_pattern  = "" # match everything; the Lambda decides what counts
  destination_arn = aws_lambda_function.invocation_cost_publisher.arn
  distribution    = "ByLogStream"

  depends_on = [aws_lambda_permission.invocation_cost_publisher_logs]
}

################################################################################
# Account contract
#
# Published under /eks-agent-platform/org/, which no cluster subtree can collide
# with: a cluster name is `<environment>-<clusterBase>` and `org` is a reserved
# environment token with no base.
#
# The operator cannot read these. Its whole configuration is one recursive
# GetParametersByPath sweep of its OWN cluster prefix, so cost-access republishes
# each handle under /eks-agent-platform/<cluster>/cost-pipeline/ — the keys the
# operator already knows, now backed by one pipeline instead of three.
################################################################################

resource "aws_ssm_parameter" "cur_bucket" {
  name  = "/eks-agent-platform/org/cost-pipeline/cur_bucket"
  type  = "String"
  value = aws_s3_bucket.cur.id
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_workgroup" {
  name  = "/eks-agent-platform/org/cost-pipeline/athena_workgroup"
  type  = "String"
  value = aws_athena_workgroup.cost.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_database" {
  name  = "/eks-agent-platform/org/cost-pipeline/athena_database"
  type  = "String"
  value = aws_glue_catalog_database.cost.name
  tags  = local.tags
}

resource "aws_ssm_parameter" "cur_table_name" {
  name  = "/eks-agent-platform/org/cost-pipeline/cur_table_name"
  type  = "String"
  value = local.cur_table_name
  tags  = local.tags
}

resource "aws_ssm_parameter" "estimate_table_name" {
  name  = "/eks-agent-platform/org/cost-pipeline/estimate_table_name"
  type  = "String"
  value = local.estimate_table_name
  tags  = local.tags
}

resource "aws_ssm_parameter" "reconciliation_view" {
  name  = "/eks-agent-platform/org/cost-pipeline/reconciliation_view"
  type  = "String"
  value = local.reconciliation_view
  tags  = local.tags
}

################################################################################
# PlatformId cost-allocation tag — account-global, not retroactive, and the reason
# the CUR leg of every budget can read zero while every query succeeds.
#
# Activating this key is what makes `resource_tags_user_platform_id` a column in the
# report at all. Until it is active the column is absent from the Parquet, so the
# reconciler's predicate matches nothing and every tenant reads zero spend with a
# query that ran and returned. Nothing goes red.
#
# ─── why terraform asserts this rather than owning it ───
#
# AWS offers a user-defined key for activation only once it has OBSERVED a resource
# carrying it, then takes up to 24h to list it and up to 24h more to activate it. So
# the activation cannot happen in the same apply that first stamps the key. That much
# is a sequencing problem, and a count gate would be the obvious answer.
#
# It is not, because terraform cannot see any of the three things it would need:
#
#   The gate has no signal. `aws_ce_tags` — the only Cost Explorer data source that
#   exists — reads cost DATA dimensions, and a user-defined key appears there only
#   once it is ALREADY an active cost allocation tag. Verified against the live
#   account: it returns 3 keys, every one of them Active, with zero overlap against
#   the 1048 Inactive ones. So "wait until the key is observed, then activate it"
#   waits for the thing it is trying to cause.
#
#   Nothing can see an inactive key. `ListCostAllocationTags` is the API that lists
#   what is available to activate, and the provider ships no data source for it.
#
#   And the resource cannot report its own failure. `aws_ce_cost_allocation_tag`
#   creates with `_, err := conn.UpdateCostAllocationTagsStatus(ctx, input)`, and
#   that API returns HTTP 200 with a per-key `TagKeysNotFoundException` inside an
#   `Errors` array which the `_` discards. Activating a key AWS has never seen
#   reports `1 added` and does nothing at all.
#
# A resource that cannot be gated, cannot be verified, and reports success when it
# no-ops is not ownership; it is the failure class this pipeline exists to remove,
# declared in HCL. So the activation is a one-time human act, and what lives here is
# the assertion that it happened.
#
# The same data source that makes a useless gate makes an exact verifier: because it
# returns only activated keys, presence IS activation. This component still stamps
# PlatformId on its own buckets, which is what puts the key into AWS's inventory so a
# human has something to activate.
################################################################################

data "aws_ce_tags" "observed" {
  provider = aws.us_east_1

  # plantimestamp(), not timestamp(). timestamp() is an APPLY-time value, so the
  # provider cannot read this data source during plan, `tags` comes back "known after
  # apply", and a count derived from it is rejected outright — the component becomes
  # permanently unplannable with a message telling the operator to run with -exclude.
  # Verified against the live account rather than reasoned about: with timestamp() the
  # plan errors on Invalid count argument; with plantimestamp() it resolves.
  time_period {
    start = formatdate("YYYY-MM-DD", timeadd(plantimestamp(), "-720h"))
    end   = formatdate("YYYY-MM-DD", plantimestamp())
  }
}

locals {
  # Both keys, because they attribute different halves of the bill and neither covers
  # the other.
  #
  #   PlatformId              a RESOURCE tag, carried by the tenant's datastores. It
  #                           reaches the CUR's resource_tags map and is how buckets,
  #                           databases and queues attribute.
  #
  #   iamPrincipal/PlatformId an IAM PRINCIPAL tag, carried by the tenant's role. A
  #                           model invocation is not a taggable resource, so no
  #                           resource tag is ever populated on one — AWS attributes
  #                           Bedrock spend by the calling identity instead, and this
  #                           is the only column that can see it.
  #
  # Activating one and not the other produces a budget that is confidently wrong in a
  # specific direction rather than obviously broken.
  required_cost_allocation_tags = ["PlatformId", "iamPrincipal/PlatformId"]

  inactive_cost_allocation_tags = [
    for k in local.required_cost_allocation_tags :
    k if !contains(data.aws_ce_tags.observed.tags, k)
  ]
}

check "the_cost_allocation_tags_are_active" {
  assert {
    condition = length(local.inactive_cost_allocation_tags) == 0
    error_message = <<-EOT
      These cost-allocation tag keys are not active in Cost Explorer, so the CUR carries no
      column for them and every BudgetPolicy's billed leg reads zero through a query that
      succeeds: ${join(", ", local.inactive_cost_allocation_tags)}

      Activation is a one-time act and terraform cannot do it. The provider's resource calls
      UpdateCostAllocationTagsStatus and discards the per-key error the API returns in a 200,
      so declaring it here would report success while doing nothing.

        aws ce update-cost-allocation-tags-status --region us-east-1 \
          --cost-allocation-tags-status ${join(" ", [
    for k in local.required_cost_allocation_tags : "TagKey=${k},Status=Active"
])}

      A key is only listed for activation once AWS has observed a resource carrying it —
      up to 24h — and takes up to 24h more to activate. This component stamps PlatformId on
      its own buckets so the key enters that inventory at install rather than when the first
      tenant arrives. For iamPrincipal/PlatformId the trigger is a call, not a resource: the
      key appears once a role carrying the tag has invoked Bedrock at least once.

      Activation is NOT retroactive. Spend inside that window is unattributable permanently,
      which is why this is loud on every plan rather than a silence you have to know to look
      for.
    EOT
}
}

# Handles cost-access needs to mint each cluster's operator grant. ARNs are published
# rather than left to be reconstructed from names: a consumer composing
# `arn:aws:athena:<region>:<account>:workgroup/<name>` has to get three more things
# right than it has to know, and a grant scoped to a mis-composed ARN denies silently.
resource "aws_ssm_parameter" "athena_workgroup_arn" {
  name  = "/eks-agent-platform/org/cost-pipeline/athena_workgroup_arn"
  type  = "String"
  value = aws_athena_workgroup.cost.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_database_arn" {
  name  = "/eks-agent-platform/org/cost-pipeline/athena_database_arn"
  type  = "String"
  value = aws_glue_catalog_database.cost.arn
  tags  = local.tags
}

# The operator reads cost-pipeline/athena_results_bucket from its own cluster prefix
# and nothing ever published it, so the field was permanently empty. Harmless while
# the workgroup supplies the output location, but a seam claiming more than it
# delivers — and the grant below genuinely needs the bucket.
resource "aws_ssm_parameter" "athena_results_bucket" {
  name  = "/eks-agent-platform/org/cost-pipeline/athena_results_bucket"
  type  = "String"
  value = aws_s3_bucket.athena_results.id
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_results_bucket_arn" {
  name  = "/eks-agent-platform/org/cost-pipeline/athena_results_bucket_arn"
  type  = "String"
  value = aws_s3_bucket.athena_results.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "cur_bucket_arn" {
  name  = "/eks-agent-platform/org/cost-pipeline/cur_bucket_arn"
  type  = "String"
  value = aws_s3_bucket.cur.arn
  tags  = local.tags
}

# The IAM path this component scoped the publisher's tag-read grant to. Published so
# every cluster's cost-access can compare it against that cluster's agent-iam
# contract and refuse the operator grant if they have drifted — the account cannot
# read a per-cluster parameter, but each cluster can read both.
resource "aws_ssm_parameter" "tenant_iam_path" {
  name  = "/eks-agent-platform/org/cost-pipeline/tenant_iam_path"
  type  = "String"
  value = local.tenant_iam_path
  tags  = local.tags
}
