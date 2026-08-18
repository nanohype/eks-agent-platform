locals {
  env_vars = read_terragrunt_config(find_in_parent_folders("env.hcl"))

  environment   = local.env_vars.locals.environment
  region        = local.env_vars.locals.region
  account_id    = local.env_vars.locals.account_id
  cost_center   = local.env_vars.locals.cost_center
  business_unit = local.env_vars.locals.business_unit

  # Optional, because not every root serves a cluster. Two things in this tree are
  # account+region singletons — the Bedrock invocation-logging configuration, and a
  # Cost and Usage Report, which covers the whole account and names no cluster.
  # Those live under live/org/, whose env.hcl declares no cluster because there is
  # no cluster to declare.
  #
  # Read with try() rather than a sentinel value. A placeholder cluster_name would
  # satisfy the parse and then flow into resource names and IAM paths as if it meant
  # something, which is how an account-scoped resource ends up wearing a cluster's
  # identity.
  cluster_name = try(local.env_vars.locals.cluster_name, null)
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

# Cost and Usage Reports and Cost Explorer are us-east-1-only APIs. A component that
# reaches them declares `configuration_aliases = [aws.us_east_1]` and takes this;
# a component that does not simply ignores it.
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"
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

# The region every root declares — and therefore the region of the state bucket
# named below — is fixed to us-east-1 by organization policy, not by preference.
# The Ventures OU carries an SCP (`guardrail-region-lock`, declared in
# landing-zone's `organization` component) that denies every non-global action
# whose `aws:RequestedRegion` is anything else. Only the global services are
# carved out: iam, route53, cloudfront, acm, sts, organizations, billing.
#
# S3 is not among them, which is what makes this a backend concern rather than a
# resource concern. `bucket` below is interpolated from `local.region`, so a root
# that declares another region does not deploy to that region — it fails at
# backend init, before a single resource is planned, with an SCP deny on the
# state bucket itself. That failure names S3 and not the region, so it reads as a
# credentials or bucket-policy problem; hence this note.
#
# `live/org` is the one root that reads its region from AWS_REGION rather than
# pinning it, because its two objects are account+region singletons. Point it at
# us-east-1 for the same reason.
remote_state {
  backend = "s3"

  generate = {
    path      = "backend.tf"
    if_exists = "overwrite_terragrunt"
  }

  config = {
    bucket       = "eks-agent-platform-tfstate-${local.account_id}-${local.region}"
    key          = "eks-agent-platform/${path_relative_to_include()}/terraform.tfstate"
    region       = local.region
    encrypt      = true
    use_lockfile = true
  }
}

# cluster_name is merged in only when the root has one, rather than passed as null.
# A null TF_VAR is not the same as an absent one to every consumer, and a component
# that does not declare the variable would take it silently either way — so the
# account roots simply never send it.
inputs = merge(
  {
    environment = local.environment
    region      = local.region
    tags = {
      PlatformProject = "eks-agent-platform"
      Environment     = local.environment
    }
  },
  local.cluster_name == null ? {} : { cluster_name = local.cluster_name },
)
