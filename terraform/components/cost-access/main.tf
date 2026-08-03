################################################################################
# One cluster's access to the account's cost pipeline.
#
# The pipeline itself is applied once for the account (components/cost-pipeline),
# because a Cost and Usage Report has no filter and always covers the whole account.
# Two things about it are nonetheless per-cluster, and this component is both:
#
#   the operator's grant   it attaches to ONE cluster's operator role, discovered
#                          from that cluster's agent-iam contract. N clusters need N
#                          policies and N attachments.
#   the SSM handles        the operator's entire configuration is one recursive
#                          GetParametersByPath sweep of /eks-agent-platform/<cluster>/,
#                          so a parameter published under the account prefix is
#                          invisible to it. The account's handles are republished
#                          here under the keys the operator already reads — same
#                          contract, one pipeline behind it instead of three.
#
# Nothing here creates cost infrastructure. If this component is what you are editing
# to change the pipeline, you are in the wrong one.
################################################################################

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

# The operator role lives in landing-zone's canonical agent-iam component, keyed on
# this cluster. Read from the contract it publishes rather than carrying a duplicate.
data "aws_ssm_parameter" "operator_role_name" {
  name = "/eks-agent-platform/${var.cluster_name}/agent-iam/operator_role_name"
}

# The IAM path THIS cluster's operator mints roles under.
data "aws_ssm_parameter" "cluster_tenant_iam_path" {
  name = "/eks-agent-platform/${var.cluster_name}/agent-iam/tenant_iam_path"
}

################################################################################
# The account pipeline's published handles.
#
# Read from SSM rather than a terragrunt `dependency` on the account root. A
# dependency resolves at terragrunt PARSE time, so every per-cluster leaf would fail
# `init` — not `apply` — whenever the account state was absent, and no TF_VAR gets
# you out of that. landing-zone's account-scoped components are consumed the same
# way for the same reason.
################################################################################

locals {
  account_prefix = "/eks-agent-platform/org/cost-pipeline"
}

# Only what this component uses: the ARNs the operator's grant is scoped to, the two
# names the operator's query needs, and the IAM path the precondition below checks.
#
# The account publishes more than this — the CUR bucket name, the estimate table, the
# reconciliation view. Those are the account's own query surface, read by whoever
# queries the account (an analyst, a dashboard, a preflight). They are not part of the
# operator's configuration, so this component does not carry them across.
data "aws_ssm_parameter" "cur_bucket_arn" { name = "${local.account_prefix}/cur_bucket_arn" }
data "aws_ssm_parameter" "athena_workgroup" { name = "${local.account_prefix}/athena_workgroup" }
data "aws_ssm_parameter" "athena_workgroup_arn" { name = "${local.account_prefix}/athena_workgroup_arn" }
data "aws_ssm_parameter" "athena_database" { name = "${local.account_prefix}/athena_database" }
data "aws_ssm_parameter" "athena_database_arn" { name = "${local.account_prefix}/athena_database_arn" }
data "aws_ssm_parameter" "athena_results_bucket_arn" { name = "${local.account_prefix}/athena_results_bucket_arn" }
data "aws_ssm_parameter" "cur_table_name" { name = "${local.account_prefix}/cur_table_name" }
data "aws_ssm_parameter" "account_tenant_iam_path" { name = "${local.account_prefix}/tenant_iam_path" }
data "aws_ssm_parameter" "account_data_kms_key_arn" { name = "${local.account_prefix}/data_kms_key_arn" }

locals {
  prefix = "${var.cluster_name}-cost-access"

  tags = merge(var.tags, {
    Component = "cost-access"
    Tier      = "platform"

    # Which workload environment this grant belongs to. The pipeline it reaches is
    # account-scoped and carries Scope = "account"; this side is the per-cluster
    # counterpart, and an auditor reading an IAM policy in the console should be able
    # to tell which of the two they are looking at without resolving the ARN.
    Environment = var.environment
  })

  # nonsensitive() throughout: the aws_ssm_parameter data source marks every value
  # sensitive regardless of type, and the mark propagates through jsonencode into the
  # whole IAM policy document — so `tofu plan` would render this cluster's operator
  # permissions as `(sensitive value)` and hide every future change to them from
  # review. These are ARNs and resource names, public by construction.
  account = {
    cur_bucket_arn            = nonsensitive(data.aws_ssm_parameter.cur_bucket_arn.value)
    athena_workgroup_arn      = nonsensitive(data.aws_ssm_parameter.athena_workgroup_arn.value)
    athena_database_arn       = nonsensitive(data.aws_ssm_parameter.athena_database_arn.value)
    athena_database           = nonsensitive(data.aws_ssm_parameter.athena_database.value)
    athena_results_bucket_arn = nonsensitive(data.aws_ssm_parameter.athena_results_bucket_arn.value)
    tenant_iam_path           = nonsensitive(data.aws_ssm_parameter.account_tenant_iam_path.value)

    # The key the account pipeline actually encrypts with, not a second copy of it.
    # This component creates nothing encrypted — it only grants the operator use of
    # what the account already encrypted — so the account is the sole authority and
    # there is no per-cluster value that could legitimately differ.
    data_kms_key_arn = nonsensitive(data.aws_ssm_parameter.account_data_kms_key_arn.value)
  }

  cluster_tenant_iam_path = nonsensitive(data.aws_ssm_parameter.cluster_tenant_iam_path.value)

  # Normalized both sides before comparing. The path is used as a prefix, so
  # "/x/tenants" and "/x/tenants/" mean the same thing to a reader and different
  # things to IAM — comparing the raw strings would report drift that is not there,
  # and a gate that cries wolf gets bypassed.
  # trimspace before comparing. An SSM value that picked up a trailing newline in shell
  # or CI plumbing is the same path to IAM and a different string here, and this
  # precondition FAILS THE APPLY — so whitespace nobody can see would refuse a correctly
  # built cluster. A gate that cries wolf is a gate that gets bypassed, and then it is
  # not a gate.
  trimmed_cluster_path    = trimspace(local.cluster_tenant_iam_path)
  trimmed_account_path    = trimspace(local.account.tenant_iam_path)
  normalized_cluster_path = endswith(local.trimmed_cluster_path, "/") ? local.trimmed_cluster_path : "${local.trimmed_cluster_path}/"
  normalized_account_path = endswith(local.trimmed_account_path, "/") ? local.trimmed_account_path : "${local.trimmed_account_path}/"
}

################################################################################
# The operator's grant — read the CUR, run the Athena query, decrypt the results.
################################################################################

resource "aws_iam_policy" "operator_cost" {
  name = "${local.prefix}-operator"
  path = "/eks-agent-platform/"
  tags = local.tags

  lifecycle {
    # The cross-tier check the account component cannot make.
    #
    # The account's cost publisher reads the PlatformId tag off invoking roles, and
    # its grant is scoped to ONE IAM path — an account-wide constant in landing-zone,
    # but published by agent-iam per cluster, so cost-pipeline takes it as an input
    # and cannot verify it against anything. This component can: it reads both the
    # path the account scoped to and the path THIS cluster's operator actually mints
    # under, and refuses to grant access to a pipeline that cannot see its roles.
    #
    # If they drift, every invocation from this cluster attributes to "unknown" and
    # every budget here reads low — a wrong number rather than an error, which is why
    # it fails the apply instead of warning.
    precondition {
      condition     = local.normalized_account_path == local.normalized_cluster_path
      error_message = <<-EOT
        The account cost pipeline scoped its tag-read grant to a different IAM path than this
        cluster's operator mints roles under, so the publisher cannot read this cluster's
        PlatformId tags.

        Every Bedrock invocation from this cluster would attribute to "unknown": the spend is
        still published, but it reaches no Platform's budget, and every BudgetPolicy here reads
        low with nothing going red.

        Set cost-pipeline's tenant_iam_path to match agent-iam's tenant_role_path, and re-apply
        live/org/cost-pipeline before this leaf.
      EOT
    }
  }

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
        Resource = local.account.athena_workgroup_arn
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
          local.account.athena_database_arn,
          "arn:${data.aws_partition.current.partition}:glue:${var.region}:${data.aws_caller_identity.current.account_id}:table/${local.account.athena_database}/*"
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
          local.account.athena_results_bucket_arn,
          "${local.account.athena_results_bucket_arn}/*"
        ]
      },
      {
        Sid    = "CurRead"
        Effect = "Allow"
        # The CUR bucket used to grant this through its own bucket policy, naming one
        # cluster's operator. A bucket serving every cluster cannot do that — a bucket
        # policy is a single document, so N clusters would mean N writers rewriting one
        # object and the last apply deciding who still has access. Within one account
        # an identity policy is sufficient on its own.
        Action = [
          "s3:GetObject",
          "s3:ListBucket"
        ]
        Resource = [
          local.account.cur_bucket_arn,
          "${local.account.cur_bucket_arn}/*"
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
        #   GenerateDataKey - WRITE the result set. The workgroup enforces SSE_KMS
        #                     results, so this is not optional, and Decrypt alone
        #                     leaves every query failing at the write step
        #   DescribeKey     - the SDK's key-resolution path
        #
        # Without these the query returns FAILED on every tick, reconcileBudget returns
        # before the kill-switch block, and a tenant at 120% of budget never trips it
        # with nothing anywhere going red.
        Action   = ["kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
        Resource = [local.account.data_kms_key_arn]
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
  role       = nonsensitive(data.aws_ssm_parameter.operator_role_name.value)
  policy_arn = aws_iam_policy.operator_cost.arn
}

################################################################################
# The account's handles, republished under this cluster's prefix.
#
# Exactly the three keys the operator decodes, and no more. The Budget reconciler
# refuses to build a query with any of them empty, so these three are the whole of
# what the cost pipeline contributes to the operator's configuration.
#
# A republished key with no decode case is not harmless. It reads as configuration
# the operator honours, so the next person to change the pipeline changes it here
# too and watches for an effect that cannot arrive — and the parameter itself is a
# per-cluster resource that has to be created, tagged, and destroyed for nobody.
#
# What stands behind the three is one pipeline for the account, not one per
# environment.
################################################################################

resource "aws_ssm_parameter" "athena_workgroup" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_workgroup"
  type  = "String"
  value = nonsensitive(data.aws_ssm_parameter.athena_workgroup.value)
  tags  = local.tags
}

resource "aws_ssm_parameter" "athena_database" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_database"
  type  = "String"
  value = local.account.athena_database
  tags  = local.tags
}

resource "aws_ssm_parameter" "cur_table_name" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/cur_table_name"
  type  = "String"
  value = nonsensitive(data.aws_ssm_parameter.cur_table_name.value)
  tags  = local.tags
}
