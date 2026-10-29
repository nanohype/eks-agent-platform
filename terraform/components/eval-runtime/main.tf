locals {
  tags = merge(var.tags, {
    Component = "eval-runtime"
    Tier      = "platform"
  })
}

# The eval-reports bucket is owned by landing-zone's agent-iam component and
# published under the per-cluster SSM contract. Its name is read here rather
# than through a terraform input, so this component depends only on that
# contract, not on a sibling terraform state.
data "aws_ssm_parameter" "eval_reports_bucket_name" {
  name = "/eks-agent-platform/${var.cluster_name}/model-artifacts/eval_reports_bucket_name"
}

################################################################################
# No IAM here.
#
# An eval run holds its Platform's own identity, not one this component mints.
# The operator creates, per Platform, a <cluster>-<platform>-eval role under the
# tenant IAM path and permissions boundary, an eval-<platform> ServiceAccount in
# the eval runtime namespace, and the EKS Pod Identity association between them.
# That role reaches only the Platform's own prefixes in the shared eval-reports
# bucket — GetObject on <platform>/manifests/*, PutObject on <platform>/runs/* —
# and KMS only through S3 for that bucket
# (operators/internal/controller/platform_eval_identity.go).
#
# One role for every run could not draw that line: whatever it may read or write
# under one Platform's prefix, it may under every Platform's. Nor does a run need
# Bedrock. eval-runner reaches the model through the Platform's ModelGateway over
# HTTP, so a Bedrock grant here would only be a path around the per-Platform
# model scoping the operator generates.
################################################################################

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

# The two values the operator reads at startup. It submits eval Workflows into
# runner_namespace and creates each Platform's eval ServiceAccount there, and it
# scopes each Platform's eval role into eval_reports_bucket and hands the same
# bucket to every run as its upload target.
resource "aws_ssm_parameter" "eval_runner_namespace" {
  name  = "/eks-agent-platform/${var.cluster_name}/eval-runtime/runner_namespace"
  type  = "String"
  value = var.eval_runner_namespace
  tags  = local.tags
}

resource "aws_ssm_parameter" "eval_reports_bucket" {
  name  = "/eks-agent-platform/${var.cluster_name}/eval-runtime/eval_reports_bucket"
  type  = "String"
  value = data.aws_ssm_parameter.eval_reports_bucket_name.value
  tags  = local.tags
}
