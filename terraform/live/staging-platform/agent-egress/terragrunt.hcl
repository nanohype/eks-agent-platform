include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/agent-egress"
}

inputs = {
  enable_waf           = false
  agentgateway_alb_arn = ""
}
