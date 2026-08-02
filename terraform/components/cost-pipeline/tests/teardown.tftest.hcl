# Teardown posture for cost-pipeline.
#
# The contract: a permitted teardown opens every bucket at once, and a protected environment
# opens none of them. Both directions are asserted, because a gate is only correct if it holds
# in both — with only the permissive assertion, a bucket wired `force_destroy = true`
# unconditionally passes and the protection is gone with nothing to show for it.
#
# These are root-module resources, so an assert reads the attribute the provider will send
# rather than an output restating the expression that produced it.
#
# The lifecycle assertions are the other half. force_destroy makes a teardown possible;
# without an expiry that actually removes objects, these buckets grow for the life of the
# account. Both are versioned, so an `expiration` writes a delete marker that is itself the
# object's new current version — an expiry alone empties nothing, which is why every rule here
# is paired with a noncurrent-version rule and a delete-marker sweep.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/test"
      user_id    = "AIDTEST"
    }
  }
  mock_data "aws_partition" {
    defaults = {
      partition          = "aws"
      dns_suffix         = "amazonaws.com"
      reverse_dns_prefix = "com.amazonaws"
    }
  }
  mock_data "aws_ssm_parameter" {
    defaults = {
      value = "arn:aws:iam::123456789012:role/mock-operator"
    }
  }
  # Computed ARNs are randomised by the mock unless pinned, and several resources here
  # validate the shape of one they consume.
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock-role"
    }
  }
  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::123456789012:policy/mock-policy"
    }
  }
  mock_resource "aws_lambda_function" {
    defaults = {
      arn = "arn:aws:lambda:us-west-2:123456789012:function:mock-fn"
    }
  }
}

variables {
  environment                  = "staging"
  region                       = "us-west-2"
  cluster_name                 = "staging-platform"
  cur_report_name              = "eks-agent-platform-test"
  data_kms_key_arn             = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000"
  logs_kms_key_arn             = "arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111"
  bedrock_invocation_log_group = "mock-bedrock-invocations"
  tags                         = {}
}

# A protected environment with the lever unset keeps every bucket. This is the posture a real
# tenant's cost data sits in, and the direction that a permanently-open gate would break.
run "protected_environment_keeps_every_bucket" {
  command = plan

  assert {
    condition     = aws_s3_bucket.cur.force_destroy == false
    error_message = "the CUR bucket holds this environment's billing history — AWS never re-delivers a closed month, so it must not be force-destroyable without the lever"
  }
  assert {
    condition     = aws_s3_bucket.athena_results.force_destroy == false
    error_message = "athena-results must not be force-destroyable without the lever"
  }
  assert {
    condition     = aws_s3_bucket.access_logs.force_destroy == false
    error_message = "access-logs must not be force-destroyable without the lever"
  }
}

# The permitted teardown. Every bucket opens on the same act, so the reverse sweep reaches all
# of them in one destroy rather than emptying two and wedging on the third.
run "force_destroy_buckets_opens_every_bucket" {
  command = plan

  variables {
    force_destroy_buckets = true
  }

  assert {
    condition = alltrue([
      aws_s3_bucket.cur.force_destroy,
      aws_s3_bucket.athena_results.force_destroy,
      aws_s3_bucket.access_logs.force_destroy,
    ])
    error_message = "force_destroy_buckets must open all three buckets — a partial teardown empties some and then wedges on BucketNotEmpty, leaving the cluster, VPC and NAT gateways billing"
  }
}

# Development is disposable by construction and needs no flag.
run "development_is_unconditionally_tearable_down" {
  command = plan

  variables {
    environment  = "development"
    cluster_name = "development-platform"
  }

  assert {
    condition = alltrue([
      aws_s3_bucket.cur.force_destroy,
      aws_s3_bucket.athena_results.force_destroy,
      aws_s3_bucket.access_logs.force_destroy,
    ])
    error_message = "development must tear down without an opt-in — it is the environment a validation run rebuilds"
  }
}

# Versioned buckets need three things to actually empty, not one. A rule set that only expires
# current versions reads as a retention policy and delivers unbounded growth.
run "versioned_buckets_can_actually_empty" {
  command = plan

  assert {
    condition = alltrue([
      for r in aws_s3_bucket_lifecycle_configuration.athena_results.rule :
      r.status == "Enabled"
    ])
    error_message = "every athena-results lifecycle rule must be Enabled"
  }

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.athena_results.rule :
      length(r.noncurrent_version_expiration) > 0
    ])
    error_message = "athena-results is versioned: without a noncurrent_version_expiration the objects an expiry 'removes' stay as noncurrent versions forever"
  }

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.athena_results.rule :
      anytrue([for e in r.expiration : e.expired_object_delete_marker == true])
    ])
    error_message = "athena-results needs an expired-object-delete-marker sweep — a marker with nothing under it still counts as an object"
  }

  # The CUR Parquet under cur/ is the bulk of that bucket and the input to every budget
  # decision. A rule scoped only to estimates/ leaves it with no expiry at all.
  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.cur.rule :
      anytrue([for f in r.filter : f.prefix == "cur/"])
    ])
    error_message = "the CUR Parquet under cur/ must have its own expiry — an estimates/-scoped rule leaves the primary data unbounded, so the bucket is permanently non-empty"
  }

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.cur.rule :
      anytrue([for e in r.expiration : e.expired_object_delete_marker == true])
    ])
    error_message = "the CUR bucket needs an expired-object-delete-marker sweep for the same reason as athena-results"
  }
}
