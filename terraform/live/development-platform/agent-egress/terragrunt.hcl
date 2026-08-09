include "root" {
  path = find_in_parent_folders("root.hcl")
}

terraform {
  source = "${get_repo_root()}/terraform/components/agent-egress"
}

inputs = {
  enable_waf            = false
  model_gateway_alb_arn = ""

  # landing-zone's network component already provisions the s3 gateway endpoint
  # and the interface endpoint set in this VPC (both default on in create mode),
  # and a gateway endpoint's route is per route table — a second one for the same
  # service is rejected outright. Defer endpoint ownership to the VPC's owner.
  create_vpc_endpoints = false
}
