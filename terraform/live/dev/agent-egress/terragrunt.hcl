include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/agent-egress"
}

inputs = {
  vpc_id                    = "vpc-REPLACE"
  private_subnet_ids        = ["subnet-REPLACE-a", "subnet-REPLACE-b", "subnet-REPLACE-c"]
  route_table_ids           = ["rtb-REPLACE-a", "rtb-REPLACE-b", "rtb-REPLACE-c"]
  cluster_security_group_id = "sg-REPLACE"

  enable_waf           = false
  agentgateway_alb_arn = ""
}
