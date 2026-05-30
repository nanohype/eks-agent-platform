output "cur_bucket_arn" {
  description = "S3 bucket holding CUR Parquet partitions"
  value       = aws_s3_bucket.cur.arn
}

output "athena_workgroup" {
  description = "Athena workgroup used by the budget-controller"
  value       = aws_athena_workgroup.cost.name
}

output "athena_database" {
  description = "Glue catalog database containing the CUR table"
  value       = aws_glue_catalog_database.cost.name
}

output "athena_results_bucket" {
  description = "S3 bucket for Athena query results (30-day TTL)"
  value       = aws_s3_bucket.athena_results.id
}

output "cur_table_name" {
  description = "Predicted Glue table name produced by the CUR Crawler (CUR report name with hyphens normalized to underscores). Operator reads this from SSM."
  value       = local.cur_table_name
}

output "invocation_cost_publisher_function_name" {
  description = "Name of the Lambda that republishes Bedrock invocation cost as a per-PlatformId CloudWatch metric."
  value       = aws_lambda_function.invocation_cost_publisher.function_name
}

output "estimate_table_name" {
  description = "Glue table over the per-platform daily invocation-cost estimate prefix (partition projection on usage_date)."
  value       = local.estimate_table_name
}

output "reconciliation_view" {
  description = "Athena view name (estimate vs CUR truth) created by the spend_reconciliation saved query. Materialize it by running that query once in the cost workgroup."
  value       = local.reconciliation_view
}
