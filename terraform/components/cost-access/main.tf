################################################################################
# One cluster's access to the account's cost pipeline.
#
# The pipeline itself is applied once for the account (components/cost-pipeline),
# because a Cost and Usage Report covers the whole account and carries no column that
# identifies a cluster, so a per-cluster copy would be a duplicate rather than a view.
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
#   the Athena workgroup  it decides where results land and how they are encrypted,
#                         and it decides that for every caller in it. Enforced
#                         configuration means a client cannot vary the output prefix,
#                         so one workgroup for the account is one results prefix for
#                         every cluster. Same cardinality as the grant: one per
#                         caller set.
#
# Nothing here creates cost STORAGE or a catalog — no bucket, no database, no
# retention. The workgroup is a query-execution context bound to one cluster's
# identity, pointed at the account's bucket. If this component is what you are editing
# to change the pipeline itself, you are in the wrong one.
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
data "aws_ssm_parameter" "athena_results_bucket" { name = "${local.account_prefix}/athena_results_bucket" }
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
    athena_database_arn       = nonsensitive(data.aws_ssm_parameter.athena_database_arn.value)
    athena_database           = nonsensitive(data.aws_ssm_parameter.athena_database.value)
    athena_results_bucket     = nonsensitive(data.aws_ssm_parameter.athena_results_bucket.value)
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
# This cluster's Athena workgroup.
#
# One workgroup per caller set, not one per account. The workgroup decides where a
# query's results land and how they are encrypted, and it decides it for everyone who
# runs in it: enforce_workgroup_configuration means a client-supplied
# ResultConfiguration is ignored outright, so a shared workgroup gives every cluster's
# operator the same output prefix and no way to vary it. Each cluster gets its own
# prefix by getting its own workgroup — there is no other lever.
#
# The storage is still the account's. This creates no bucket, no catalog and no
# retention: it points at the results bucket cost-pipeline owns, under a prefix keyed
# by cluster name, with the account's own key.
#
# What this does NOT buy, stated plainly so nobody reads the boundary as tighter than
# it is: CUR access remains account-wide. Nothing in the export identifies a cluster —
# that is why the pipeline is account-scoped at all — so there is no per-cluster CUR
# prefix to narrow to, and the CurRead grant below still covers the whole export. This scopes
# where a cluster WRITES its own query output. It is not billing-data isolation.
################################################################################

resource "aws_athena_workgroup" "cluster" {
  name = local.prefix
  tags = local.tags

  configuration {
    # The operator sends no ResultConfiguration, so this is the only thing deciding
    # where results land and how they are encrypted. Enforced, so that stays true even
    # if some future caller does send one.
    enforce_workgroup_configuration = true

    # Per-cluster Athena metrics. With one shared workgroup, a reconciler burning
    # scanned bytes is indistinguishable from any other cluster's.
    publish_cloudwatch_metrics_enabled = true

    result_configuration {
      output_location = "s3://${local.account.athena_results_bucket}/results/${var.cluster_name}/"

      encryption_configuration {
        encryption_option = "SSE_KMS"
        kms_key_arn       = local.account.data_kms_key_arn
      }
    }
  }
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
        # This cluster's workgroup, not the account's. The pair is load-bearing: the
        # SSM handle below hands the operator this workgroup's NAME, and a grant on
        # any other workgroup ARN makes every StartQueryExecution AccessDenied.
        Resource = aws_athena_workgroup.cluster.arn
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
        Sid    = "AthenaResultObjects"
        Effect = "Allow"
        # Scoped to this cluster's own results prefix — the same prefix the workgroup
        # above enforces as the output location. The two are derived from
        # var.cluster_name on both sides so they cannot drift apart into a workgroup
        # writing where the policy does not permit.
        #
        # Athena runs S3 under the CALLER's identity, so this is what actually writes
        # the result set. Narrowing it is the point of the per-cluster workgroup: a
        # compromised operator in development can no longer overwrite the objects
        # production's reconciler reads back.
        #
        # The multipart actions are here because a result set large enough is written
        # as a multipart upload, and a write that cannot complete or clean up fails
        # after the scan — which the reconciler reports as unreadable spend rather
        # than as access denied, and a tenant over budget is never stopped.
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:AbortMultipartUpload",
          "s3:ListMultipartUploadParts"
        ]
        Resource = ["${local.account.athena_results_bucket_arn}/results/${var.cluster_name}/*"]
      },
      {
        Sid    = "AthenaResultsBucket"
        Effect = "Allow"
        # Bucket-level actions take the bucket ARN, never an object ARN — s3:ListBucket
        # on `bucket/*` matches nothing, and Athena fails at listing before it writes
        # anything. AWS's own identity-based policy example for Athena splits exactly
        # this way.
        #
        # Deliberately NOT narrowed with an s3:prefix condition. Athena's listing
        # prefix is internal and not something this component can predict; a condition
        # that guesses wrong denies every query, and the failure is the silent one
        # again. The residual is that a cluster's operator can enumerate KEY NAMES in
        # the shared results bucket — not read their contents, which the object grant
        # above scopes.
        Action = [
          "s3:ListBucket",
          "s3:GetBucketLocation",
          "s3:ListBucketMultipartUploads"
        ]
        Resource = [local.account.athena_results_bucket_arn]
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
        #   Decrypt         - read the result set back on GetQueryResults. NOT for the
        #                     estimates objects: the operator queries only the CUR
        #                     table, and holds no s3:GetObject on the estimates bucket
        #                     at all. Trimming this grant against that reading would
        #                     break every GetQueryResults instead
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

# This cluster's own workgroup, not the account's. Its pair with the AthenaQuery grant
# above is why they ship together: publishing the account workgroup while the policy
# permits only this one is AccessDenied on every tick, and creating this one while the
# handle still names the account's hands the operator a workgroup its policy does not
# cover. Either half alone produces stale budgets and a kill switch that never fires,
# with nothing red.
resource "aws_ssm_parameter" "athena_workgroup" {
  name  = "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_workgroup"
  type  = "String"
  value = aws_athena_workgroup.cluster.name
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
