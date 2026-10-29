output "eval_runner_namespace" {
  description = "Kubernetes namespace where eval Workflows run and each Platform's eval ServiceAccount lives. Matches the namespace the operator emits Workflow CRs into."
  value       = var.eval_runner_namespace
}

output "controller_log_group_name" {
  description = "CloudWatch log group for the Argo Workflows controller"
  value       = aws_cloudwatch_log_group.controller.name
}
