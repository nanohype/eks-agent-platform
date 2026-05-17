output "artifacts_bucket_arn" {
  description = "S3 bucket ARN for model artifacts (LoRA / adapter / fine-tuned weights)"
  value       = aws_s3_bucket.artifacts.arn
}

output "artifacts_bucket_name" {
  description = "S3 bucket name for model artifacts"
  value       = aws_s3_bucket.artifacts.id
}

output "eval_reports_bucket_arn" {
  description = "S3 bucket ARN for eval reports"
  value       = aws_s3_bucket.eval_reports.arn
}

output "eval_reports_bucket_name" {
  description = "S3 bucket name for eval reports"
  value       = aws_s3_bucket.eval_reports.id
}
