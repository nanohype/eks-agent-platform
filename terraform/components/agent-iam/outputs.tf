output "operator_role_arn" {
  description = "IRSA role assumed by the operator pod"
  value       = aws_iam_role.operator.arn
}

output "operator_role_name" {
  description = "Operator IRSA role name"
  value       = aws_iam_role.operator.name
}

output "tenant_iam_path" {
  description = "IAM path under which per-tenant roles are created"
  value       = var.tenant_iam_path
}

output "tenant_baseline_policy_arn" {
  description = "ARN of the baseline policy attached to every tenant role"
  value       = aws_iam_policy.tenant_baseline.arn
}
