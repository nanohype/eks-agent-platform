locals {
  env_vars = read_terragrunt_config(find_in_parent_folders("env.hcl"))

  environment   = local.env_vars.locals.environment
  region        = local.env_vars.locals.region
  cluster_name  = local.env_vars.locals.cluster_name
  account_id    = local.env_vars.locals.account_id
  cost_center   = local.env_vars.locals.cost_center
  business_unit = local.env_vars.locals.business_unit
}

generate "provider" {
  path      = "provider.tf"
  if_exists = "overwrite_terragrunt"
  contents  = <<EOF
provider "aws" {
  region = "${local.region}"
  default_tags {
    tags = {
      Environment  = "${local.environment}"
      ManagedBy    = "opentofu"
      Project      = "eks-agent-platform"
      CostCenter   = "${local.cost_center}"
      BusinessUnit = "${local.business_unit}"
      Repository   = "nanohype/eks-agent-platform"
    }
  }
}
EOF
}

remote_state {
  backend = "s3"

  generate = {
    path      = "backend.tf"
    if_exists = "overwrite_terragrunt"
  }

  config = {
    bucket         = "eks-agent-platform-tfstate-${local.account_id}-${local.region}"
    key            = "eks-agent-platform/${path_relative_to_include()}/terraform.tfstate"
    region         = local.region
    encrypt        = true
    use_lockfile   = true
  }
}

inputs = {
  environment  = local.environment
  region       = local.region
  cluster_name = local.cluster_name
  tags = {
    PlatformProject = "eks-agent-platform"
    Environment     = local.environment
  }
}
