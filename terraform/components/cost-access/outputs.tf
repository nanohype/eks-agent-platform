output "operator_cost_policy_arn" {
  description = "IAM policy granting this cluster's operator role read access to the account cost pipeline."
  value       = aws_iam_policy.operator_cost.arn
}

output "athena_database" {
  description = "Glue database holding the account's CUR and estimate tables, republished under this cluster's SSM prefix."
  value       = local.account.athena_database
}
