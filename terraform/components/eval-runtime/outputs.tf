output "eval_runner_role_arn" {
  description = "IRSA role ARN for the eval-runner ServiceAccount. Annotated on the SA by the gitops/addons/eval-runtime kustomize package."
  value       = aws_iam_role.eval_runner.arn
}

output "eval_runner_role_name" {
  description = "IRSA role name for the eval-runner ServiceAccount"
  value       = aws_iam_role.eval_runner.name
}

output "eval_runner_namespace" {
  description = "Kubernetes namespace where Argo Workflow pods run. Matches the namespace the operator emits Workflow CRs into."
  value       = var.eval_runner_namespace
}

output "eval_runner_service_account" {
  description = "ServiceAccount name the Workflows reference"
  value       = var.eval_runner_service_account
}

output "controller_log_group_name" {
  description = "CloudWatch log group for the Argo Workflows controller"
  value       = aws_cloudwatch_log_group.controller.name
}
