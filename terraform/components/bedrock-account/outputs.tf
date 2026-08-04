################################################################################
# Account contract
#
# Published to SSM under /eks-agent-platform/org/, not handed over by a terragrunt
# `dependency`. Two reasons, and the second is the load-bearing one:
#
#   a dependency across roots would make every per-environment leaf's config PARSE
#     depend on the account root's state existing. terragrunt resolves dependencies
#     at parse time, so a missing account state fails `init`, not `apply`, and no
#     TF_VAR gets you out of it. landing-zone's account-scoped components consume
#     nothing this way for exactly that reason.
#   the operator reads SSM anyway. Its entire configuration is one recursive
#     GetParametersByPath sweep, so SSM is already the contract surface — publishing
#     here means the same mechanism carries both tiers.
#
# The `org` segment cannot collide with a cluster subtree: a cluster name is
# `<environment>-<clusterBase>`, and `org` is a reserved environment token with no
# base, so no cluster can be called it.
#
# Note what is NOT published: nothing here is readable by the operator, because the
# operator only sweeps its own cluster's prefix. cost-access is what republishes the
# handles a cluster needs under that cluster's path.
################################################################################

locals {
  ssm_prefix = "/eks-agent-platform/org/bedrock-account"
}

resource "aws_ssm_parameter" "invocation_log_group" {
  name = "${local.ssm_prefix}/invocation_log_group"
  type = "String"
  # The cost publisher subscribes to this. It lives in cost-pipeline, which is a
  # separate account-scoped root, so this parameter is the join between them.
  value = aws_cloudwatch_log_group.invocations.name
  tags  = local.tags
}

# The ARNs of both are NOT published. cost-pipeline is the only consumer of either, it
# needs the log group's NAME to attach a subscription filter, and it composes the ARN it
# grants on from that name. A published ARN nobody reads is a contract with one side, and
# this component had two of them. They remain module outputs below, where a caller that
# wants them can take them.

output "invocation_bucket_arn" {
  description = "S3 bucket ARN receiving Bedrock invocation logs for the whole account."
  value       = aws_s3_bucket.invocations.arn
}

output "invocation_bucket_name" {
  description = "S3 bucket name receiving Bedrock invocation logs for the whole account."
  value       = aws_s3_bucket.invocations.id
}

output "invocation_log_group_name" {
  description = "CloudWatch log group receiving Bedrock invocation logs for the whole account. The cost publisher in cost-pipeline subscribes to it."
  value       = aws_cloudwatch_log_group.invocations.name
}

output "invocation_log_group_arn" {
  description = "ARN of the account-wide Bedrock invocation log group."
  value       = aws_cloudwatch_log_group.invocations.arn
}

output "bedrock_logging_role_arn" {
  description = "IAM role Bedrock assumes to deliver invocation logs."
  value       = aws_iam_role.bedrock_logging.arn
}

output "access_logs_bucket_name" {
  description = "S3 bucket receiving server-access logs from the invocations bucket."
  value       = aws_s3_bucket.access_logs.id
}
