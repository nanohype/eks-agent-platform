output "event_bus_name" {
  description = "Custom EventBridge bus the operator publishes BudgetBreach events to"
  value       = aws_cloudwatch_event_bus.killswitch.name
}

output "event_bus_arn" {
  description = "Custom EventBridge bus ARN"
  value       = aws_cloudwatch_event_bus.killswitch.arn
}

output "state_machine_arn" {
  description = "Step Functions state machine that executes the kill-switch"
  value       = aws_sfn_state_machine.killswitch.arn
}

output "archive_name" {
  description = "EventBridge archive for compliance retention"
  value       = aws_cloudwatch_event_archive.killswitch.name
}
