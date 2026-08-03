# The invocation-logging outputs are gone, not renamed. Their resources are
# account+region singletons owned by bedrock-account, and re-exporting them from a
# per-cluster component would hand consumers a per-cluster handle for something
# there is only one of — which is how three environments came to each believe they
# owned the account's logging.
#
# A consumer that needs the log group reads the account contract at
# /eks-agent-platform/org/bedrock-account/. There is no per-cluster copy: the only
# thing that sweeps a cluster prefix is the operator, and it has no consumer for
# either handle.

output "baseline_guardrail_id" {
  description = "ID of the baseline Bedrock Guardrail (null when disabled OR when applied in a region without Bedrock Guardrails support)"
  value       = local.enable_guardrail ? aws_bedrock_guardrail.baseline[0].guardrail_id : null
}

output "baseline_guardrail_version" {
  description = "Version of the baseline Bedrock Guardrail (null when disabled or unsupported in region)"
  value       = local.enable_guardrail ? aws_bedrock_guardrail.baseline[0].version : null
}
