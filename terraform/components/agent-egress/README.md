# components/agent-egress

PrivateLink endpoints + (optional) WAF for the agent data plane.

- **Interface endpoints**: `bedrock-runtime`, `bedrock`, `bedrock-agent-runtime`, `sts`, `secretsmanager`, `logs`, `monitoring`, `ssm`, `kms`. Private DNS enabled. Single shared security group accepts 443 from the cluster security group.
- **Gateway endpoints**: `s3`, `dynamodb`. Attached to private + intra route tables.
- **WAF** (opt-in via `enable_waf`): AWS managed Common + KnownBadInputs rule groups + per-IP rate limit (2000 req/5min). Attached to the agentgateway ALB.

All Bedrock invocations from in-cluster agents traverse PrivateLink — no traffic leaves AWS via the public internet.

## Inputs

| Variable                                          | Description                                  |
| ------------------------------------------------- | -------------------------------------------- |
| `environment`, `region`, `cluster_name`           | identifying                                  |
| `vpc_id`, `private_subnet_ids`, `route_table_ids` | from landing-zone network outputs            |
| `cluster_security_group_id`                       | from landing-zone cluster outputs            |
| `enable_waf`                                      | toggle the ALB WAF                           |
| `agentgateway_alb_arn`                            | ALB to associate (required when WAF enabled) |

## Outputs

- `endpoint_security_group_id`
- `interface_endpoint_ids` (map)
- `gateway_endpoint_ids` (map)
- `waf_web_acl_arn`
