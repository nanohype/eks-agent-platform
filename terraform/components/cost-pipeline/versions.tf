terraform {
  required_version = ">= 1.11.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
      # Cost and Usage Reports and Cost Explorer are us-east-1-only APIs, whatever
      # region the substrate runs in. The report definition and the cost-allocation
      # tag activation are created through this alias; everything else — the buckets,
      # the workgroup, the Lambda — uses the default provider in the workload region.
      configuration_aliases = [aws.us_east_1]
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.4"
    }
  }
}
