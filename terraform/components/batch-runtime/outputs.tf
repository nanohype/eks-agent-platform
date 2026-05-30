output "batch_service_role_arn" {
  description = "Bedrock batch service-role ARN. The operator resolves this from SSM and passes it as the BatchJob's RoleArn."
  value       = aws_iam_role.batch.arn
}

output "batch_service_role_name" {
  description = "Bedrock batch service-role name."
  value       = aws_iam_role.batch.name
}

output "batch_prefix" {
  description = "S3 key prefix (under the model-artifacts bucket) batch input/output JSONL is staged beneath, and the service role is scoped to."
  value       = local.batch_prefix
}
