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
  data_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111"
  tags             = {}
}

# A protected environment with the lever unset keeps every bucket. This is the posture a real
# tenant's cost data sits in, and the direction that a permanently-open gate would break.
run "protected_environment_keeps_every_bucket" {
  command = plan

  assert {
    condition     = aws_s3_bucket.estimates.force_destroy == false
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
      aws_s3_bucket.estimates.force_destroy,
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
      aws_s3_bucket.estimates.force_destroy == false,
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

  # The estimate objects are the bulk of that bucket now that the report lives in
  # landing-zone. The rule that names them must be enabled AND expire something AND clear
  # the noncurrent versions its expiry creates — all three on the same rule.
  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.estimates.rule :
      anytrue([for f in r.filter : f.prefix == "estimates/"])
      && r.status == "Enabled"
      && anytrue([for e in r.expiration : e.days > 0])
      && length(r.noncurrent_version_expiration) > 0
    ])
    error_message = "the estimates/ rule must be enabled, expire objects, and clear the noncurrent versions that expiry creates — a filter alone leaves the data unbounded and the bucket permanently non-empty"
  }

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.estimates.rule :
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
      for cfg in [aws_s3_bucket_lifecycle_configuration.estimates, aws_s3_bucket_lifecycle_configuration.athena_results] :
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
      [for r in aws_s3_bucket_lifecycle_configuration.estimates.rule : r.status == "Enabled"],
      [for r in aws_s3_bucket_lifecycle_configuration.athena_results.rule : r.status == "Enabled"],
      [for r in aws_s3_bucket_lifecycle_configuration.access_logs.rule : r.status == "Enabled"],
    ))
    error_message = "a Disabled lifecycle rule applies nothing while reading as a retention policy"
  }
}

# The crawler reads where landing-zone actually delivers.
#
# The report is not this component's any more — org-cost owns the account's CUR 2.0
# export and publishes its location over SSM. This component reads all three parts and
# composes the path, and a wrong part does not fail: the crawler finds no objects,
# registers a table with no partitions, and every query against it returns zero rows and
# exits zero. schema_change_policy delete_behavior is LOG, so a previously-good table
# would also just sit there stale.
#
# Each contract read gets its own sentinel, because the account handles are all the same
# shape — three parameters resolving to one mock default cannot tell a correct path from
# a transposed one.
run "the_cur_table_reads_where_the_export_is_delivered" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.cur_export_bucket
    values = { value = "SENTINEL-bucket" }
  }
  override_data {
    target = data.aws_ssm_parameter.cur_export_prefix
    values = { value = "SENTINEL-prefix" }
  }
  override_data {
    target = data.aws_ssm_parameter.cur_export_name
    values = { value = "SENTINEL-name" }
  }

  # AWS delivers to <prefix>/<export-name>/data/BILLING_PERIOD=YYYY-MM/ with a SIBLING
  # metadata/ prefix holding manifest JSON. The table has to sit on `data` specifically:
  # one level up covers the JSON too, which is neither Parquet nor this schema.
  assert {
    condition     = one(aws_glue_catalog_table.cur.storage_descriptor).location == "s3://SENTINEL-bucket/SENTINEL-prefix/SENTINEL-name/data/"
    error_message = "the CUR table's location must be the export's data/ prefix, composed from all three published parts — a location anywhere else is a table over no objects, and every budget query returns zero spend rather than failing"
  }

  # Stated separately from the equality above, because this is the specific way it was
  # wrong: pointed at the export ROOT rather than at data/ underneath it. An equality
  # that someone "fixes" by editing the expected string would still pass; this will not.
  assert {
    condition = alltrue([
      endswith(one(aws_glue_catalog_table.cur.storage_descriptor).location, "/data/"),
      one(aws_glue_catalog_table.cur.storage_descriptor).location != local.cur_export_location,
    ])
    error_message = "the CUR table must sit on the export's data/ folder, not on the export root — the root holds the metadata/ manifests alongside the Parquet, and a table spanning both has neither schema"
  }

  # The projection template and the location must address the same prefix. Athena builds
  # every partition location from the template, so a template that disagrees with the
  # location sends every query to a prefix with no objects in it — zero rows, exit zero.
  assert {
    condition = alltrue([
      startswith(aws_glue_catalog_table.cur.parameters["storage.location.template"], one(aws_glue_catalog_table.cur.storage_descriptor).location),
      strcontains(aws_glue_catalog_table.cur.parameters["storage.location.template"], "BILLING_PERIOD=$${billing_period}"),
      one(aws_glue_catalog_table.cur.partition_keys).name == "billing_period",
    ])
    error_message = "the projection template must extend the table's own location and spell the BILLING_PERIOD= key the export actually writes, and the partition key it substitutes must be the one declared"
  }

  # The name the operator queries is the name of the table this component declares —
  # the same string, not a second expression that happens to agree today.
  #
  # This is the assertion the previous shape of this test could not make. It asserted
  # the published value equalled the export name with hyphens swapped for underscores,
  # which is exactly what main.tf computed — so it agreed with the implementation no
  # matter what Glue would actually have named the table.
  assert {
    condition     = aws_ssm_parameter.cur_table_name.value == aws_glue_catalog_table.cur.name
    error_message = "the published CUR table name must BE the declared table's name — the operator queries it by that name, and a name nothing creates makes every budget query FAIL, which reconcileBudget records as unreadable spend and returns before the kill-switch block"
  }

  # And the name must not be a function of the export. Deriving it there is what made the
  # old name a prediction: landing-zone renames the export, every operator repoints at a
  # table that does not exist yet, and no apply in this repo has to change for that to
  # happen. The sentinel carries a hyphen so any such derivation is visible here.
  assert {
    condition = alltrue([
      aws_ssm_parameter.cur_table_name.value != "SENTINEL_name",
      aws_ssm_parameter.cur_table_name.value != "SENTINEL-name",
      !strcontains(aws_ssm_parameter.cur_table_name.value, "SENTINEL"),
    ])
    error_message = "the CUR table name must be this component's own constant, not derived from the export's name — a name that tracks the export means a rename in landing-zone silently repoints every operator at a table Glue has not created"
  }
}

# Partition projection is what makes a billing period queryable the moment it is
# delivered. The failure it removes is silent and monthly: with registered partitions,
# a new BILLING_PERIOD does not exist in the catalog until something registers it, the
# month-to-date query matches no rows, and COALESCE(SUM(...), 0) hands the reconciler a
# clean $0 for every tenant. Budgets read healthy and the kill switch cannot fire.
run "every_billing_period_is_queryable_without_anything_having_to_run" {
  command = plan

  assert {
    condition     = aws_glue_catalog_table.cur.parameters["projection.enabled"] == "true"
    error_message = "projection must be enabled on the CUR table — without it Athena reads the catalog's registered partition list, which nothing in this component populates, and every query returns zero rows"
  }

  # The range must stay open at the top. A fixed end date passes every other assertion
  # here and reads correctly until the clock crosses it, after which the current month
  # is outside the projected range and every budget silently reads $0.
  assert {
    condition = alltrue([
      endswith(aws_glue_catalog_table.cur.parameters["projection.billing_period.range"], ",NOW"),
      aws_glue_catalog_table.cur.parameters["projection.billing_period.type"] == "date",
      aws_glue_catalog_table.cur.parameters["projection.billing_period.format"] == "yyyy-MM",
      aws_glue_catalog_table.cur.parameters["projection.billing_period.interval.unit"] == "MONTHS",
    ])
    error_message = "the billing-period projection must run to NOW at a monthly interval in the export's own yyyy-MM format — a closed range or a mismatched format puts the current month outside the projection, and month-to-date spend reads zero with nothing going red"
  }
}

# The export carries all 125 CUR 2.0 columns; this table declares the handful the org
# queries. That subset is only legal because Athena resolves Parquet columns by NAME.
run "the_cur_table_resolves_columns_by_name" {
  command = plan

  assert {
    condition     = one(one(aws_glue_catalog_table.cur.storage_descriptor).ser_de_info).parameters["parquet.column.index.access"] == "false"
    error_message = "parquet.column.index.access must be false — set true, the declared subset resolves by ORDINAL instead, line_item_unblended_cost reads whatever column happens to be first in the file, and the query succeeds while returning a number that is not spend"
  }

  assert {
    condition     = one(one(aws_glue_catalog_table.cur.storage_descriptor).ser_de_info).serialization_library == "org.apache.hadoop.hive.ql.io.parquet.serde.ParquetHiveSerDe"
    error_message = "the CUR table must use the Parquet SerDe — the export is delivered as Parquet, and any other SerDe reads the files as something they are not"
  }
}
