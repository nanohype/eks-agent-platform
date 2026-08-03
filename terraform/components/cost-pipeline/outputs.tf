output "cur_export_bucket" {
  description = "The account CUR 2.0 export bucket this pipeline queries. landing-zone's org-cost owns it; this component only reads it."
  value       = local.cur_export_bucket
}

output "estimates_bucket_arn" {
  description = "S3 bucket holding the invocation-cost publisher's per-batch estimate objects."
  value       = aws_s3_bucket.estimates.arn
}

output "athena_workgroup" {
  description = "The ACCOUNT's Athena workgroup — the query surface the reconciliation named query binds to, and where an analyst runs against the cost database. Not what any cluster's budget reconciler uses: each cluster runs in its own workgroup, minted by cost-access, writing to its own results prefix. Wiring a dashboard or a preflight to this one and expecting reconciler activity gets a workgroup whose query metrics stay at zero."
  value       = aws_athena_workgroup.cost.name
}

output "athena_database" {
  description = "Glue catalog database holding the CUR table and the estimate table"
  value       = aws_glue_catalog_database.cost.name
}

output "athena_results_bucket" {
  description = "S3 bucket for Athena query results (30-day TTL)"
  value       = aws_s3_bucket.athena_results.id
}

output "cur_table_name" {
  description = "Glue table declared over the account export's data prefix. The name is this component's own, not a function of the export's name — the operator reads it from SSM and queries it by that name, so it is a value rather than a prediction of what some discovery step would have called it."
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
