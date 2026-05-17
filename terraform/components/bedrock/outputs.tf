output "invocation_bucket_arn" {
  description = "S3 bucket ARN for Bedrock invocation logs"
  value       = aws_s3_bucket.invocations.arn
}

output "invocation_bucket_name" {
  description = "S3 bucket name for Bedrock invocation logs"
  value       = aws_s3_bucket.invocations.id
}

output "invocation_log_group_name" {
  description = "CloudWatch log group for Bedrock invocations"
  value       = aws_cloudwatch_log_group.invocations.name
}

output "invocation_log_group_arn" {
  description = "CloudWatch log group ARN for Bedrock invocations"
  value       = aws_cloudwatch_log_group.invocations.arn
}

output "bedrock_logging_role_arn" {
  description = "IAM role assumed by Bedrock for invocation logging"
  value       = aws_iam_role.bedrock_logging.arn
}

output "baseline_guardrail_id" {
  description = "ID of the baseline Bedrock Guardrail (null when disabled OR when applied in a region without Bedrock Guardrails support)"
  value       = local.enable_guardrail ? aws_bedrock_guardrail.baseline[0].guardrail_id : null
}

output "baseline_guardrail_version" {
  description = "Version of the baseline Bedrock Guardrail (null when disabled or unsupported in region)"
  value       = local.enable_guardrail ? aws_bedrock_guardrail.baseline[0].version : null
}
