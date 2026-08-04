terraform {
  required_version = ">= 1.11.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
      # Cost Explorer has no endpoint outside us-east-1, whatever region the substrate
      # runs in, and data.aws_ce_tags is read through this alias — it is the check that
      # both cost-allocation keys are active. Everything else here is regional and uses
      # the default provider.
      #
      # Declared as a configuration_alias rather than a provider block because the
      # provider configuration belongs to whoever applies this root — terragrunt
      # generates it from live/root.hcl — and a component that configured its own would
      # be deciding credentials and region for its caller.
      #
      # It is not a gate. `tofu validate` passes with no aws.us_east_1 configured
      # anywhere (checked, not assumed); the absence surfaces when the provider is
      # actually needed. And it is not assertable from a run block either: `provider` is
      # a meta argument, not a resource attribute, so an assertion phrased around it
      # would pass with the argument deleted.
      configuration_aliases = [aws.us_east_1]
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.4"
    }
  }
}
