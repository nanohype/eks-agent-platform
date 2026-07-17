include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${dirname(find_in_parent_folders("root.hcl"))}/../components/agent-egress"
}

# Required inputs sourced from the orchestrator (portal workspace
# variables for the production deploy):
#   - vpc_id, private_subnet_ids, route_table_ids  (from lz-network)
#   - cluster_security_group_id                    (from lz-cluster)
inputs = {
  enable_waf           = false
  agentgateway_alb_arn = ""

  # lz-network already provisioned s3/sts/ssm/secretsmanager/kms/logs/ecr/
  # eks/dynamodb endpoints in this VPC; defer endpoint ownership to it.
  create_vpc_endpoints = false
}
