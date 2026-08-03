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
  mock_data "aws_region" {
    defaults = {
      region = "us-west-2"
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

# The component reaches the us-east-1-only CUR and Cost Explorer APIs through this
# alias; nothing in this suite exercises it, but a declared configuration_alias must
# be satisfied for the plan to build at all.
mock_provider "aws" {
  alias = "us_east_1"

  # The component carries a check block asserting both cost-allocation keys are active,
  # and a firing check fails the run. This suite is about teardown posture, so it
  # defaults to a correctly configured account rather than tripping on an unrelated
  # concern.
  mock_data "aws_ce_tags" {
    defaults = {
      tags = ["PlatformId", "iamPrincipal/PlatformId", "CostCenter", "BusinessUnit"]
    }
  }
}

variables {
  region           = "us-west-2"
  cur_report_name  = "eks-agent-platform-test"
  data_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111"
  tags             = {}
}

# A protected environment with the lever unset keeps every bucket. This is the posture a real
# tenant's cost data sits in, and the direction that a permanently-open gate would break.
run "protected_environment_keeps_every_bucket" {
  command = plan

  assert {
    condition     = aws_s3_bucket.cur.force_destroy == false
    error_message = "the CUR bucket holds this environment's billing history, and AWS only re-delivers a month while it is inside its refresh window — so it must not be force-destroyable without the lever"
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
# There is no environment-shortcut run here any more, and its absence is the point.
#
# The per-environment version of this component treated `development` as
# unconditionally destroyable, because a validation run rebuilds it. Applied once for
# the ACCOUNT, there is no development: this pipeline holds the billing history every
# environment's budget reads, so nothing about it is disposable by default. The only
# lever is the explicit one, asserted above.
run "no_environment_token_can_open_the_teardown_gate" {
  command = plan

  # force_destroy_buckets defaults false, and nothing else may set it. Reaching this
  # via an environment token is precisely what stopped being possible.
  assert {
    condition = alltrue([
      aws_s3_bucket.cur.force_destroy == false,
      aws_s3_bucket.athena_results.force_destroy == false,
      aws_s3_bucket.access_logs.force_destroy == false,
    ])
    error_message = "with no explicit force_destroy_buckets, an account-scoped pipeline must keep every bucket — there is no environment here whose disposability could justify opening the gate"
  }
}

# Versioned buckets need three things to actually empty, not one. A rule set that only expires
# current versions reads as a retention policy and delivers unbounded growth.
#
# Each assertion binds the RULE THAT DOES THE WORK, not the presence of a shape somewhere in
# the list. Asserting "some rule has a noncurrent_version_expiration" passes on a decoy rule
# scoped to a prefix nothing is written under; asserting "some rule filters on cur/" passes on
# a Disabled rule, or one with no expiration at all — which is the exact defect this suite was
# written to prevent.
run "versioned_buckets_can_actually_empty" {
  command = plan

  # The CUR Parquet under cur/ is the bulk of that bucket and the input to every budget
  # decision. The rule that names it must be enabled AND expire something AND clear the
  # noncurrent versions its expiry creates — all three on the same rule.
  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.cur.rule :
      anytrue([for f in r.filter : f.prefix == "cur/"])
      && r.status == "Enabled"
      && anytrue([for e in r.expiration : e.days > 0])
      && length(r.noncurrent_version_expiration) > 0
    ])
    error_message = "the cur/ rule must be enabled, expire objects, and clear the noncurrent versions that expiry creates — a filter alone leaves the primary data unbounded and the bucket permanently non-empty"
  }

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.cur.rule :
      anytrue([for f in r.filter : f.prefix == "estimates/"])
      && r.status == "Enabled"
      && anytrue([for e in r.expiration : e.days > 0])
      && length(r.noncurrent_version_expiration) > 0
    ])
    error_message = "the estimates/ rule must be enabled, expire objects, and clear noncurrent versions"
  }

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.athena_results.rule :
      r.status == "Enabled"
      && anytrue([for e in r.expiration : e.days > 0])
      && length(r.noncurrent_version_expiration) > 0
    ])
    error_message = "athena-results is versioned: the rule that expires results must also clear the noncurrent versions that expiry creates, or the objects an expiry 'removes' stay forever"
  }

  # The delete-marker sweep must be its own rule. S3 rejects days/date in the same expiration
  # block as the flag, and a mocked plan does not catch that — so the shape is asserted here.
  assert {
    condition = alltrue([
      for cfg in [aws_s3_bucket_lifecycle_configuration.cur, aws_s3_bucket_lifecycle_configuration.athena_results] :
      anytrue([
        for r in cfg.rule :
        r.status == "Enabled" && anytrue([
          for e in r.expiration :
          e.expired_object_delete_marker == true && e.days == 0 && e.date == null
        ])
      ])
    ])
    error_message = "each versioned bucket needs a delete-marker sweep in a rule of its own — a marker with nothing under it still counts as an object, and S3 rejects days/date in the same expiration block as the flag"
  }

  # Every rule must be live. A Disabled rule is inert and reads as a retention policy.
  assert {
    condition = alltrue(concat(
      [for r in aws_s3_bucket_lifecycle_configuration.cur.rule : r.status == "Enabled"],
      [for r in aws_s3_bucket_lifecycle_configuration.athena_results.rule : r.status == "Enabled"],
      [for r in aws_s3_bucket_lifecycle_configuration.access_logs.rule : r.status == "Enabled"],
    ))
    error_message = "a Disabled lifecycle rule applies nothing while reading as a retention policy"
  }
}

# The pair, not the literal. AWS delivers the CUR Parquet under the report definition's
# s3_prefix; the Glue crawler is what registers it as the Athena table the budget reconciler
# queries. Nothing in the tree made those two agree — the crawler carried its own copy of the
# prefix, so changing where AWS delivers left the crawler reading an empty path, the stale
# table registered (schema_change_policy delete_behavior is LOG), and every BudgetPolicy
# reading zero spend forever, with a green plan and a green apply.
#
# Asserted as a relation between two resources so it cannot be satisfied by a copied constant.
run "the_crawler_reads_where_the_report_is_delivered" {
  command = plan

  assert {
    condition     = one(aws_glue_crawler.cur.s3_target).path == "s3://${aws_s3_bucket.cur.id}/${aws_cur_report_definition.this.s3_prefix}/${var.cur_report_name}/"
    error_message = "the Glue crawler must crawl exactly where the CUR report definition delivers — a crawler pointed anywhere else registers nothing, leaves the stale table in place, and the budget reconciler reads zero spend with nothing going red"
  }
}
