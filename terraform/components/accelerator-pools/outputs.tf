output "neuron_role_arn" {
  description = "IRSA role for the AWS Neuron device plugin"
  value       = aws_iam_role.neuron.arn
}

output "gpu_operator_role_arn" {
  description = "IRSA role for the NVIDIA GPU Operator"
  value       = aws_iam_role.gpu_operator.arn
}

output "pool_catalog_ssm_path" {
  description = "SSM parameter holding the JSON catalog of accelerator pool defaults"
  value       = aws_ssm_parameter.pool_catalog.name
}
