terraform {
  required_version = ">= 1.11.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
      # No us_east_1 configuration_alias. Everything this component owns is
      # account+REGION scoped and reachable through the default provider; the only
      # us-east-1-only API in this tree is Cost Explorer, and the cost-allocation
      # tag that needs it lives in cost-pipeline.
    }
  }
}
