# The budget path, asserted end to end at plan time.
#
# The kill switch is the product's central promise, and every link in the chain it
# depends on is a pair of things that never reference each other: a key the workgroup
# enforces and a grant the operator holds, a column a query names and a column the
# table declares, a prefix a Lambda writes to and a location a table reads from, a log
# group one component publishes and another subscribes to. Each pair is valid on both
# sides while being wrong between them, and every one of those failures is quiet — a
# query that returns zero rows, a Lambda that is never invoked, a metric with no
# datapoints. Nothing goes red; the number just reads low, or reads zero, which is
# indistinguishable from a tenant that did not spend anything.
#
# So the assertions here are relations, not values. A run that pins a literal proves
# the literal; a run that binds one resource's attribute to another's proves the pair
# cannot drift. Where a fixture default would make two sides of a pair look identical,
# the run overrides one with a sentinel first — that is why the region, the partition,
# the SSM handles and the bucket identities are all overridden below rather than left
# to the mock.

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

mock_provider "aws" {
  alias = "us_east_1"

  # A check block fails the run that triggers it, so the default here is a correctly
  # configured account — BOTH cost-allocation keys active. Runs that care about an
  # unactivated state override it and declare the failure they expect.
  #
  # Both keys, not just PlatformId: the resource tag attributes datastores and the
  # principal tag attributes model invocations, and a default carrying only the first
  # would make every unrelated run in this file fail for a reason none of them are
  # about.
  mock_data "aws_ce_tags" {
    defaults = {
      tags = ["PlatformId", "iamPrincipal/PlatformId", "CostCenter", "BusinessUnit"]
    }
  }
}

variables {
  data_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111"
  tags             = {}
}

# No AWS service delivers into this component's buckets — landing-zone owns the export
# — so nothing here has to tolerate a service principal that cannot use a CMK, and every
# bucket that holds cost data defaults to the account key.
run "every_bucket_holding_cost_data_defaults_to_the_account_key" {
  command = plan

  # Reached by index rather than by a comprehension over each nested block.
  #
  # `alltrue([])` is TRUE, so `alltrue([for d in <optional block> : ...])` passes when
  # the block is DELETED — which is the single edit that removes the behaviour the
  # assertion exists to protect. A value mutation cannot see that class; only deleting
  # the block can, and against the old form deleting it was green. try() collapses a
  # missing block to null instead, which matches nothing.
  assert {
    condition = try(
      one(aws_s3_bucket_server_side_encryption_configuration.estimates.rule).apply_server_side_encryption_by_default[0].sse_algorithm,
      null
    ) == "aws:kms"
    error_message = "the estimates bucket must default to SSE-KMS — the publisher sets its own header per object, so the default governs nothing on the live path and everything on any other, and SSE-S3 there would make an object written by anything but the Lambda weaker than the object beside it"
  }

  # The publisher's own header, which is what actually encrypts the objects the
  # reconciliation reads. Value-checked against the published key in the run below;
  # here it only has to be present, because a publisher handed no key writes under the
  # bucket default and that is a silent downgrade rather than a failure.
  assert {
    condition     = contains(keys(aws_lambda_function.invocation_cost_publisher.environment[0].variables), "ESTIMATE_KMS_KEY_ID")
    error_message = "the cost publisher must be handed a CMK for its own writes"
  }

  # The access-logs bucket is the exception, and stays one: S3 log delivery is an AWS
  # service principal writing into it, and it holds request metadata rather than cost
  # data.
  assert {
    condition = try(
      one(aws_s3_bucket_server_side_encryption_configuration.access_logs.rule).apply_server_side_encryption_by_default[0].sse_algorithm,
      null
    ) == "AES256"
    error_message = "the access-logs bucket must stay on SSE-S3 — logging.s3.amazonaws.com delivers into it, and it is the one bucket here whose writer is an AWS service rather than this component"
  }
}

# One key, published, and it is the one every encryption site here actually uses.
#
# cost-access scopes the operator's kms grant to whatever this parameter carries, and it
# has no way to check that against anything — the account is the only authority on which
# key encrypts cost data. So the parameter has to be the same value the workgroup
# enforces, the results bucket defaults to, and the publisher stamps on each estimate.
# Publishing any one of the four independently is a grant on a key nothing uses.
#
# The failure is silent and lands on the WRITE, not the read: the workgroup enforces
# SSE-KMS results, so a query whose caller cannot GenerateDataKey on that key fails after
# scanning, and the reconciler reports unreadable spend rather than access denied.
#
# Asserted against a distinct sentinel rather than the fixture default, because the
# suites on both sides of this contract previously carried the SAME literal ARN — two
# hardcoded values that agree prove nothing about the wiring between them.
run "the_published_cost_key_is_the_one_this_pipeline_encrypts_with" {
  command = plan

  variables {
    data_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/SENTINEL-ACCOUNT-COST-KEY"
  }

  assert {
    condition = alltrue([
      aws_ssm_parameter.data_kms_key_arn.value == "arn:aws:kms:us-west-2:123456789012:key/SENTINEL-ACCOUNT-COST-KEY",
      one(one(aws_athena_workgroup.cost.configuration).result_configuration).encryption_configuration[0].kms_key_arn == aws_ssm_parameter.data_kms_key_arn.value,
      one(one(aws_s3_bucket_server_side_encryption_configuration.athena_results.rule).apply_server_side_encryption_by_default).kms_master_key_id == aws_ssm_parameter.data_kms_key_arn.value,
      one(one(aws_s3_bucket_server_side_encryption_configuration.estimates.rule).apply_server_side_encryption_by_default).kms_master_key_id == aws_ssm_parameter.data_kms_key_arn.value,
      aws_lambda_function.invocation_cost_publisher.environment[0].variables["ESTIMATE_KMS_KEY_ID"] == aws_ssm_parameter.data_kms_key_arn.value,
    ])
    error_message = "the published data_kms_key_arn must be the key the workgroup enforces, both cost buckets default to, and the publisher stamps on estimates — every cluster's operator grant is scoped to this parameter and nothing downstream can check it, so a key published here that is not the key in use leaves every query failing at the SSE-KMS write with the reconciler reporting unreadable spend rather than access denied"
  }

  # And the key the publisher can actually USE. The grant is what turns the header
  # above into an object: without GenerateDataKey on this key every PutObject fails on
  # the KMS call, which _write_estimates logs and swallows — the handler still returns
  # status=ok, the metric path is untouched, and the estimates table is simply empty.
  #
  # The publisher does count those failures now (EstimateExportFailures), so the state is
  # observable rather than invisible. It is still not RED anywhere: no alarm in this repo
  # watches that metric, and a counter nobody reads is only better than nothing once
  # somebody goes looking. That is why the grant is asserted here rather than left to be
  # noticed in the account.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.invocation_cost_publisher.policy).Statement :
      contains(s.Action, "kms:GenerateDataKey") && contains(tolist(s.Resource), aws_ssm_parameter.data_kms_key_arn.value)
    ])
    error_message = "the publisher must hold kms:GenerateDataKey on the key it stamps on every estimate object — a grant on any other key leaves every estimate write failing inside a swallowed exception, so the export goes to zero with the Lambda reporting success on every batch"
  }

  assert {
    condition     = aws_ssm_parameter.data_kms_key_arn.name == "/eks-agent-platform/org/cost-pipeline/data_kms_key_arn"
    error_message = "the key must be published on the exact account-contract path cost-access reads — a near-miss leaves that data source unresolvable and every per-cluster cost-access apply failing on ParameterNotFound"
  }
}

# The publisher attributes spend by READING the PlatformId tag off the invoking
# role, not by taking the role's name apart. That makes one IAM permission
# load-bearing for the entire in-flight cost signal: without iam:ListRoleTags every
# lookup raises, every invocation attributes to "unknown", and every tenant's
# in-flight spend is zero — with the Lambda running, the metric publishing, and
# nothing anywhere going red.
#
# The previous arrangement needed no permission because it derived the value
# locally, and derived it wrong for the whole life of the code. A grant that can be
# forgotten is the cost of not having a contract to get wrong; asserting it here is
# what makes that trade safe.
run "the_publisher_can_read_the_tag_it_attributes_by" {
  command = plan

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.invocation_cost_publisher.policy).Statement :
      contains(s.Action, "iam:ListRoleTags")
    ])
    error_message = "the cost publisher must hold iam:ListRoleTags — it reads the PlatformId tag off the invoking role, and without the grant every invocation is attributed to 'unknown' and every budget reads zero"
  }

  # Read-only and path-scoped. A tag read that reaches every role in the account
  # is a wider grant than the job needs, and this Lambda is subscribed to a log
  # group carrying every Bedrock invocation in the account.
  #
  # Asserted as "every resource is under the operator's IAM path", not as "is not
  # one particular over-broad spelling". A blocklist of bad values passes for every
  # value it forgot — `Resource = ["*"]` is broader than `:role/*` and would sail
  # through a check that only rejects the latter.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.invocation_cost_publisher.policy).Statement :
      !contains(s.Action, "iam:ListRoleTags") || alltrue([
        for r in tolist(s.Resource) : startswith(r, "arn:aws:iam::123456789012:role${local.tenant_iam_path}")
      ])
    ])
    error_message = "the tag-read grant must be scoped to the operator's IAM path — every resource in the statement must sit under it, so a wildcard broader than the path (including a bare \"*\") fails here rather than granting a log-firehose Lambda tag-read over every role in the account"
  }

  # No environment or cluster token reaches the Lambda. One would be a second place
  # the identity is decided, which is exactly the arrangement that produced a
  # dimension disagreeing with every reader.
  assert {
    condition = length(setintersection(
      keys(aws_lambda_function.invocation_cost_publisher.environment[0].variables),
      ["AGENTS_ENVIRONMENT", "AGENTS_CLUSTER_NAME", "CLUSTER_NAME", "ENVIRONMENT"]
    )) == 0
    error_message = "the publisher must not be handed an environment or cluster token — it reads the identity from the role's tag, and a second source for the same value is what let the published dimension drift away from what the reconciler queries"
  }
}

# The grant's ARN is built by concatenating an SSM-supplied IAM path, and the path is a
# PREFIX on both sides of a contract: terraform scopes the grant by it, the operator
# creates roles under it. The operator appends a missing trailing slash before using it
# (platform_iam.go, platform_session_iam.go); if terraform does not, the same stored value
# means two different things and the grant matches nothing the operator ever creates.
#
# The path is an input now — agent-iam publishes it per CLUSTER and this component is
# applied once for the account, so there is no single subtree to read. These runs drive
# the two shapes that matter through that input.
run "the_tag_read_grant_survives_a_path_without_a_trailing_slash" {
  command = plan

  variables {
    tenant_iam_path = "/eks-agent-platform/tenants"
  }

  assert {
    condition     = local.tenant_iam_path == "/eks-agent-platform/tenants/"
    error_message = "a path stored without a trailing slash must be normalized before it is used as an ARN prefix — otherwise the grant reads role/eks-agent-platform/tenants* while the operator creates roles under /eks-agent-platform/tenants/, so every tag read is AccessDenied and every invocation attributes to 'unknown'"
  }

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.invocation_cost_publisher.policy).Statement :
      contains(s.Action, "iam:ListRoleTags")
      && contains(tolist(s.Resource), "arn:aws:iam::123456789012:role/eks-agent-platform/tenants/*")
    ])
    error_message = "the rendered grant ARN must be exactly the role path the operator mints under, with the wildcard directly after the trailing slash"
  }
}

run "the_tag_read_grant_is_unchanged_by_a_path_that_already_ends_in_a_slash" {
  command = plan

  variables {
    tenant_iam_path = "/eks-agent-platform/tenants/"
  }

  # Normalization must be idempotent — a double slash is a different path to IAM, and
  # would miss just as completely as a missing one.
  assert {
    condition     = local.tenant_iam_path == "/eks-agent-platform/tenants/"
    error_message = "normalizing an already-normalized path must not append a second slash"
  }
}

# The account root is not a path. `/` satisfies "absolute", normalizes to `/`, and
# renders the grant as `:role/*` — iam:ListRoleTags over every role in the account, on
# a Lambda subscribed to a log group carrying every Bedrock invocation.
#
# This is broader than the `Resource = ["*"]` form asserted against above, and it gets
# there through a value shaped like a path rather than like a wildcard, so the
# over-broad check cannot see it: `startswith(r, ":role/")` is trivially true. The
# variable's own validation is the only place it can be stopped, which is why the
# assertion lives on the variable.
#
# expect_failures names the variable, and tenant_iam_path carries exactly one
# validation, so this cannot pass on some unrelated failure the way a resource with
# several preconditions can.
run "the_account_root_is_not_an_acceptable_tenant_path" {
  command = plan

  variables {
    tenant_iam_path = "/"
  }

  expect_failures = [var.tenant_iam_path]
}

run "a_path_with_an_empty_segment_is_refused" {
  command = plan

  variables {
    tenant_iam_path = "/eks-agent-platform//tenants/"
  }

  expect_failures = [var.tenant_iam_path]
}

# The CUR platform-tag column, asserted where terraform composes SQL. The operator derives
# the same name in Go from the same tag key; both are pinned to AWS's published transform
# independently, on purpose — a shared derivation would let one wrong transform satisfy both
# sides and the query would still return nothing.
#
# Everything that reads CUR has to filter on the same column. The reconciliation view and the
# saved reconciliation query are the finance-facing consumers, and they were on a different
# spelling from the reconciler, so one of the two was always reading zero.
run "every_cur_consumer_filters_on_the_column_aws_produces" {
  command = plan

  assert {
    condition = alltrue([
      strcontains(local.cur_platform_tag_expr, "element_at(tags, 'resourceTags/PlatformId')"),
      strcontains(local.cur_platform_tag_expr, "element_at(tags, 'iamPrincipal/PlatformId')"),
      !strcontains(local.cur_platform_tag_expr, "tags['"),
    ])
    error_message = "CUR 2.0 carries ONE `tags` column of type map<string,string> holding every tag source keyed by prefix, so attribution is element_at(tags, '<prefix>/PlatformId') over both prefixes — the resource prefix for datastores and the principal prefix for model invocations. A flattened per-tag column is CUR 1.0 and does not exist in this export at all, and the Trino subscript form raises on a row whose map lacks the key rather than returning NULL"
  }

  # The reconciliation view is created by this saved query, so its SQL is the finance-facing
  # consumer. It has to name the same expression the reconciler does, or one of the two reads
  # zero while the other reads correctly and the disagreement is silent.
  #
  # EVERY occurrence, not merely one somewhere. The view names the platform expression three
  # times — the SELECT, the WHERE and the GROUP BY — and a `strcontains` is satisfied by any
  # single one of them. A view that SELECTs the union but FILTERS on `resourceTags/` alone
  # drops every model invocation, because an invocation is not a taggable resource and so
  # carries no resource tag at all; the view still *contains* the correct expression, and the
  # containment check still passes. That is the exact shape this component was reshaped to
  # remove, reintroduced one clause deeper.
  #
  # Not reasoned about — measured. The containment form stayed green under precisely that
  # mutation, which is why it is no longer the assertion.
  #
  # The invariant: wherever the query names either prefix, it names both, as one COALESCE.
  assert {
    condition = alltrue([
      (length(split(local.cur_platform_tag_expr, aws_athena_named_query.spend_reconciliation.query)) ==
      length(split("'resourceTags/PlatformId'", aws_athena_named_query.spend_reconciliation.query))),
      (length(split(local.cur_platform_tag_expr, aws_athena_named_query.spend_reconciliation.query)) ==
      length(split("'iamPrincipal/PlatformId'", aws_athena_named_query.spend_reconciliation.query))),
      strcontains(aws_athena_named_query.spend_reconciliation.query, local.cur_platform_tag_expr),
    ])
    error_message = "the reconciliation view names one PlatformId prefix somewhere the other is absent — a clause filtering on resourceTags/ alone drops every model invocation (not a taggable resource, so never resource-tagged) and one filtering on iamPrincipal/ alone drops every datastore, and either way the view renders as a reconciliation that found no disagreement"
  }

  # `product` is a map in CUR 2.0 as well, so the marketplace filter carries the same two
  # failure modes as the tag expression: the CUR 1.0 flattened column does not exist in this
  # export at all, and the Trino subscript RAISES on a row whose product map lacks the key
  # rather than returning NULL. Anthropic models bill as marketplace products rather than as
  # AmazonBedrock, so this predicate is what makes the dominant spend visible.
  assert {
    condition = alltrue([
      strcontains(aws_athena_named_query.spend_reconciliation.query, "element_at(product, 'product_name')"),
      !strcontains(aws_athena_named_query.spend_reconciliation.query, "product_product_name"),
      !strcontains(aws_athena_named_query.spend_reconciliation.query, "product['"),
    ])
    error_message = "the reconciliation view must read the product name out of CUR 2.0's product map with element_at — the flattened product_product_name column is CUR 1.0 and does not exist here, and the subscript form raises on any row whose map lacks the key, failing the whole query"
  }

  # The CUR 1.0 flattened spellings, refused explicitly. They are what a query written
  # against the older export shape reaches for, and neither column exists here — Athena
  # fails on an unknown column, which the reconciler records as unreadable spend.
  assert {
    condition = alltrue([
      !strcontains(aws_athena_named_query.spend_reconciliation.query, "resource_tags_user_platformid"),
      !strcontains(aws_athena_named_query.spend_reconciliation.query, "resource_tags_user_platform_id"),
    ])
    error_message = "the reconciliation view must not name a flattened per-tag CUR 1.0 column — this is a CUR 2.0 export where every tag source lives in the `tags` map, so the flat column is simply absent"
  }
}

# The table declares a SUBSET of the export's 125 columns — the ones this org queries. That
# is legal because Athena resolves Parquet by name, and it means a query naming a column the
# table does not declare is not a schema mismatch Athena forgives: the query FAILS, and a
# failed query is unreadable spend, which holds the budget at its last value.
#
# The column list is not restated here. It is EXTRACTED from the query text, so a clause
# added to the view that reaches for a new CUR column fails this run until the column is
# declared — which is the drift a hand-maintained list in a test cannot see.
run "the_cur_table_declares_every_column_its_consumers_read" {
  command = plan

  assert {
    condition = alltrue([
      for col in distinct(regexall("line_item_[a-z_]+", aws_athena_named_query.spend_reconciliation.query)) :
      contains([for c in one(aws_glue_catalog_table.cur.storage_descriptor).columns : c.name], col)
    ])
    error_message = "the reconciliation view reads a line_item_* column the CUR table does not declare — Athena fails the query rather than returning null for it, and the reconciler records a failed query as unreadable spend rather than as an error"
  }

  # The two map columns are reached through element_at() rather than by bare name, so the
  # extraction above cannot see them. Named explicitly, and asserted to be maps: declared as
  # `string`, element_at() fails the query outright.
  assert {
    condition = alltrue([
      for col in ["tags", "product"] :
      contains([
        for c in one(aws_glue_catalog_table.cur.storage_descriptor).columns : c.name
        if c.type == "map<string,string>"
      ], col)
    ])
    error_message = "tags and product must be declared as map<string,string> — every platform attribution and the marketplace product filter go through element_at() on those maps, and a column declared as a string there fails the whole query"
  }

  # The extraction is only a gate if it actually found something. An empty regexall makes
  # alltrue() vacuously true, so a query rewritten to reference no line_item column at all
  # would pass the first assertion silently.
  assert {
    condition     = length(distinct(regexall("line_item_[a-z_]+", aws_athena_named_query.spend_reconciliation.query))) >= 3
    error_message = "the reconciliation view no longer references the line_item columns its CUR leg is built from — alltrue([]) is true, so the column check above passes vacuously when the query stops naming any of them"
  }

  # And each leg has to aggregate the column it claims to. The check above is satisfied by a
  # query that merely MENTIONS the line_item columns somewhere — measured: replacing
  # SUM(line_item_unblended_cost) with SUM(1) left every other assertion in this file green,
  # because the WHERE clause still names three of them. That view would publish a row COUNT
  # in a column called cur_truth_usd, and every delta against the estimate would be noise
  # presented as reconciliation.
  assert {
    condition = alltrue([
      length(regexall("SUM\\(line_item_unblended_cost\\)", aws_athena_named_query.spend_reconciliation.query)) == 1,
      length(regexall("SUM\\(estimate_usd\\)", aws_athena_named_query.spend_reconciliation.query)) == 1,
    ])
    error_message = "each leg of the reconciliation view must SUM the amount column it is named for — the CUR leg on line_item_unblended_cost and the estimate leg on estimate_usd. Aggregating anything else still materializes a view, and finance reads whatever it produces as dollars"
  }

  # The cost column specifically, with its type. Declared as a string it would still be
  # extracted and still be declared, and SUM() over it fails at query time.
  assert {
    condition = contains([
      for c in one(aws_glue_catalog_table.cur.storage_descriptor).columns : c.name
      if c.type == "double"
    ], "line_item_unblended_cost")
    error_message = "line_item_unblended_cost must be declared as double — it is what both the budget query and the reconciliation view SUM, and summing a string column fails the query rather than reading zero"
  }

  assert {
    condition = contains([
      for c in one(aws_glue_catalog_table.cur.storage_descriptor).columns : c.name
      if c.type == "timestamp"
    ], "line_item_usage_start_date")
    error_message = "line_item_usage_start_date must be declared as timestamp — the budget query's month-to-date window compares it against date_trunc('month', current_date), which a string column does not support"
  }
}

# A table is only reachable through the database it was created in, and the operator is
# told which database to use by a separate SSM publish. Three things have to name the same
# Glue database — the table, the published handle, and the saved query — and nothing was
# asserting any of them. A table created in one database while the operator is pointed at
# another is not a broken apply: it is a query against a database where that table does not
# exist, which fails, which the reconciler records as unreadable spend.
run "the_table_the_operator_is_sent_to_is_the_one_that_holds_it" {
  command = plan

  assert {
    condition = alltrue([
      aws_glue_catalog_table.cur.database_name == aws_glue_catalog_database.cost.name,
      aws_glue_catalog_table.estimates.database_name == aws_glue_catalog_database.cost.name,
      aws_athena_named_query.spend_reconciliation.database == aws_glue_catalog_database.cost.name,
      aws_ssm_parameter.athena_database.value == aws_glue_catalog_database.cost.name,
    ])
    error_message = "the CUR table, the estimates table, the reconciliation query and the published athena_database handle must all name the same Glue database — the operator resolves database and table from two separate parameters, so a table in a different database than the one published is a query against a table that is not there"
  }

  # The saved query's FROM clauses, extracted rather than restated. Both legs of the
  # reconciliation read a table this component declares; a FROM repointed at anything else
  # produces a view that is valid, materializes, and reconciles the wrong thing.
  assert {
    condition = alltrue([
      length(flatten(regexall("FROM ([a-z_0-9]+)", aws_athena_named_query.spend_reconciliation.query))) == 2,
      contains(flatten(regexall("FROM ([a-z_0-9]+)", aws_athena_named_query.spend_reconciliation.query)), aws_glue_catalog_table.cur.name),
      contains(flatten(regexall("FROM ([a-z_0-9]+)", aws_athena_named_query.spend_reconciliation.query)), aws_glue_catalog_table.estimates.name),
    ])
    error_message = "the reconciliation view must read its CUR leg from the declared CUR table and its estimate leg from the declared estimates table — pointed at anything else it still materializes, and finance reads a reconciliation of something other than what it names"
  }
}

# NOTE on the us-east-1 provider. Cost Explorer has no endpoint outside us-east-1, so
# data.aws_ce_tags is read through `aws.us_east_1`. That binding is NOT assertable here — the
# `provider` meta argument is not a resource attribute, so a run block cannot see it, and an
# assertion phrased around it would pass with the argument deleted. Nor does the
# `configuration_aliases` declaration in versions.tf catch it: `tofu validate` passes with no
# such provider configured anywhere. Said out loud because the obvious-looking assertion is the
# vacuous one, and because there is no non-vacuous one to write in its place.

# The workgroup enforces encrypted results on the same key the operator is granted
# above. Asserted as a relation so the pair cannot drift apart: an enforced key the
# operator cannot use is the exact defect this suite exists to prevent.
run "the_workgroup_enforces_the_key_the_operator_holds" {
  command = plan

  # Three claims, three assertions, each reached by index.
  #
  # This was one condition nesting a comprehension per nested block, and every one of
  # those blocks is optional. `alltrue([])` is TRUE, so deleting encryption_configuration
  # from the workgroup left this green while the workgroup stopped enforcing SSE-KMS on
  # results — the run's entire subject. Split apart and indexed, a missing block reads
  # null and fails the specific claim that depended on it.
  assert {
    condition     = try(one(aws_athena_workgroup.cost.configuration).enforce_workgroup_configuration, false)
    error_message = "the workgroup must enforce its own configuration — without it a caller that sends its own ResultConfiguration decides where results land and how they are encrypted, and every assertion below describes a default nobody has to take"
  }

  assert {
    condition = try(
      one(one(aws_athena_workgroup.cost.configuration).result_configuration).encryption_configuration[0].encryption_option,
      null
    ) == "SSE_KMS"
    error_message = "the workgroup must enforce SSE_KMS on result sets — with no encryption_configuration at all the results land unencrypted and nothing here goes red"
  }

  assert {
    condition = try(
      one(one(aws_athena_workgroup.cost.configuration).result_configuration).encryption_configuration[0].kms_key_arn,
      null
    ) == var.data_kms_key_arn
    error_message = "the workgroup must enforce results on data_kms_key_arn. The other half of this pair — that each cluster's operator actually holds Decrypt AND GenerateDataKey on that key — is asserted in cost-access, which is where the grant is minted; enforcing a key the caller cannot use fails every query at the write step"
  }
}

# ── The subscription arm ──────────────────────────────────────────────────────
#
# The publisher only ever runs because CloudWatch Logs invokes it, and three separate
# things have to name the same log group for that to happen: the subscription filter
# attaches to it, the lambda permission authorizes an invoke whose SourceArn is it, and
# bedrock-account is what decides what it is called. None of the three referenced any
# of the others in any assertion.
#
# Every failure on this arm reads as a quiet account. CloudWatch Logs authorizes the
# destination when the filter is CREATED, so a permission that stops matching afterwards
# leaves a filter that still exists and still reports healthy while its invokes are
# refused — no Lambda error metric, because the Lambda never runs. The reconciler's
# GetMetricData then succeeds with no datapoints, which queryInflightCost returns as
# "0" with a nil error. A dead arm and an idle tenant produce the same number.
#
# Both sentinels are needed and neither is optional. The SSM mock serves ONE default
# value to all four parameters this component reads, so without an override the log
# group name is indistinguishable from the CUR export bucket; and the fixture's region
# has to differ from every literal elsewhere in this file, or an assertion cannot tell
# a value derived from the provider from one somebody typed.
run "the_publisher_is_wired_to_the_log_group_the_account_publishes" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.bedrock_invocation_log_group
    values = { value = "/aws/bedrock/SENTINEL-account/invocations" }
  }

  override_data {
    target = data.aws_region.current
    values = { region = "eu-north-1" }
  }

  # The partition too, for the same reason: it is a component of the composed ARN, and
  # a literal `aws` typed into it is invisible in a fixture whose partition is `aws`.
  override_data {
    target = data.aws_partition.current
    values = { partition = "aws-us-gov" }
  }

  assert {
    condition     = aws_cloudwatch_log_subscription_filter.invocations.log_group_name == "/aws/bedrock/SENTINEL-account/invocations"
    error_message = "the subscription filter must attach to the log group bedrock-account published, read from its SSM contract — a filter on any other group is created successfully against a group that carries no Bedrock invocations, and the in-flight cost leg reads zero for every tenant"
  }

  # Stated as a relation between the two resources, not as a second literal. They are
  # 200 lines apart and one composes an ARN from what the other takes as a name, which
  # is exactly the pair that drifts.
  assert {
    condition     = strcontains(aws_lambda_permission.invocation_cost_publisher_logs.source_arn, ":log-group:${aws_cloudwatch_log_subscription_filter.invocations.log_group_name}:")
    error_message = "the lambda permission must authorize invokes from the SAME log group the subscription filter attaches to — CloudWatch Logs sends the subscribed group's ARN as the SourceArn, so a permission naming a different one denies every invoke, and the filter that was authorized at create time goes on existing with nothing calling the Lambda"
  }

  # The region in that ARN, and in the service principal, is the PROVIDER's — the same
  # one the filter is created in. This is the assertion the old fixture could not make:
  # it pinned the mocked region and the region variable to the same literal, so a value
  # taken from either source looked identical.
  assert {
    condition = alltrue([
      aws_lambda_permission.invocation_cost_publisher_logs.principal == "logs.eu-north-1.amazonaws.com",
      startswith(aws_lambda_permission.invocation_cost_publisher_logs.source_arn, "arn:aws-us-gov:logs:eu-north-1:123456789012:log-group:"),
    ])
    error_message = "the permission's principal and SourceArn must carry the partition, region and account the providers are configured with — every part typed as a literal instead is invisible in an account where the literal happens to be right, and wrong everywhere else, and the mismatch only ever shows up as invokes that never arrive"
  }

  assert {
    condition     = aws_lambda_permission.invocation_cost_publisher_logs.source_account == "123456789012"
    error_message = "the permission must state the source account alongside the ARN — the CloudWatch Logs service principal is regional but not account-scoped, so without it the only account binding is a substring of the ARN"
  }

  assert {
    condition     = aws_cloudwatch_log_subscription_filter.invocations.destination_arn == aws_lambda_function.invocation_cost_publisher.arn
    error_message = "the subscription filter must deliver to this component's publisher"
  }

  # Same region, one more place. The estimates bucket is created by the provider, so an
  # S3-mediated KMS request from it carries the provider's region — a ViaService built
  # from anywhere else denies every estimate write, and _write_estimates swallows it.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.invocation_cost_publisher.policy).Statement :
      !contains(s.Action, "kms:GenerateDataKey") || try(s.Condition.StringEquals["kms:ViaService"], []) == ["s3.eu-north-1.amazonaws.com"]
    ])
    error_message = "the publisher's KMS grant must be conditioned on S3 in the PROVIDER's region — the bucket it writes to lives there, so a condition naming any other region matches no request the publisher makes and every estimate write dies inside a swallowed exception"
  }
}

# ── The estimate export's address ─────────────────────────────────────────────
#
# The Lambda composes an object key from two environment variables. The Glue table
# projects partition locations from two parameters. Nothing connects them, and a
# disagreement is not an error on either side: the publisher keeps writing into a
# prefix nothing reads, Athena keeps projecting locations that hold no objects, and the
# reconciliation view LEFT JOINs an empty estimate leg onto CUR and renders as a
# reconciliation that found nothing to disagree about.
#
# Each bucket is overridden to its own identity, and that is not decoration. tofu
# generates mock values per ATTRIBUTE, not per resource, so with no override every
# aws_s3_bucket in the component reports the same id — measured, not assumed: both this
# bucket and the results bucket come back as "rJNrtZl". Under that default, handing the
# publisher the results bucket instead of the estimates bucket is a swap between two
# equal strings, and the assertion below cannot see it. It was a MISS on exactly that
# mutation before these overrides existed.
run "the_estimate_export_writes_where_the_table_reads" {
  command = plan

  override_resource {
    target = aws_s3_bucket.estimates
    values = {
      id  = "SENTINEL-estimates-bucket"
      arn = "arn:aws:s3:::SENTINEL-estimates-bucket"
    }
  }

  override_resource {
    target = aws_s3_bucket.athena_results
    values = {
      id  = "SENTINEL-athena-results-bucket"
      arn = "arn:aws:s3:::SENTINEL-athena-results-bucket"
    }
  }

  assert {
    condition = alltrue([
      aws_s3_bucket.estimates.id == "SENTINEL-estimates-bucket",
      aws_s3_bucket.athena_results.id == "SENTINEL-athena-results-bucket",
    ])
    error_message = "the two buckets must resolve to distinct identities in this run, or every assertion below compares one generated string against itself"
  }

  # Which bucket the estimates sub-resources are ATTACHED to, which only this run can
  # ask. Everywhere else they are reached by terraform address — `.estimates` — and the
  # address is a name in this file, not a claim about the account: repoint the `bucket`
  # argument at the results bucket and the resource is still called `.estimates`, so
  # every assertion that names it keeps passing while the estimates bucket has no such
  # configuration at all. Measured: the whole suite stays green under that edit without
  # these three.
  assert {
    condition = alltrue([
      aws_s3_bucket_server_side_encryption_configuration.estimates.bucket == aws_s3_bucket.estimates.id,
      aws_s3_bucket_lifecycle_configuration.estimates.bucket == aws_s3_bucket.estimates.id,
      aws_s3_bucket_public_access_block.estimates.bucket == aws_s3_bucket.estimates.id,
      aws_s3_bucket_versioning.estimates.bucket == aws_s3_bucket.estimates.id,
    ])
    error_message = "the estimates bucket's encryption, retention, public-access block and versioning must all attach to the estimates bucket — misattached, the resource keeps its terraform name and every assertion elsewhere that reaches it by that name goes on passing, while the real bucket carries none of them"
  }

  # The same question for the results bucket, because the misattachment has two ends:
  # a sub-resource pointed at the wrong bucket leaves one bucket bare AND gives the
  # other a second, competing configuration.
  assert {
    condition = alltrue([
      aws_s3_bucket_server_side_encryption_configuration.athena_results.bucket == aws_s3_bucket.athena_results.id,
      aws_s3_bucket_lifecycle_configuration.athena_results.bucket == aws_s3_bucket.athena_results.id,
      aws_s3_bucket_public_access_block.athena_results.bucket == aws_s3_bucket.athena_results.id,
    ])
    error_message = "the results bucket's own sub-resources must attach to the results bucket"
  }

  assert {
    condition     = aws_lambda_function.invocation_cost_publisher.environment[0].variables["ESTIMATE_BUCKET"] == aws_s3_bucket.estimates.id
    error_message = "the publisher must be handed this component's estimates bucket — with no ESTIMATE_BUCKET at all the export disables itself silently, and with the wrong one it writes somewhere nothing reads"
  }

  # The table's location IS the publisher's bucket and prefix. Composed from the
  # environment the Lambda is actually given rather than from the locals both happen to
  # read today, so editing either side alone fails here.
  assert {
    condition = one(aws_glue_catalog_table.estimates.storage_descriptor).location == format(
      "s3://%s/%s/",
      aws_lambda_function.invocation_cost_publisher.environment[0].variables["ESTIMATE_BUCKET"],
      aws_lambda_function.invocation_cost_publisher.environment[0].variables["ESTIMATE_PREFIX"],
    )
    error_message = "the estimates table must sit on exactly the bucket and prefix the publisher is configured to write to — a table over a prefix with no objects returns zero rows and exits zero, and the view reports that as agreement"
  }

  # And the projection template has to extend that location with the partition
  # directory the publisher actually spells. Athena builds every partition location
  # from the template, so a template that disagrees with the location sends every query
  # to a prefix that was never written to.
  assert {
    condition = aws_glue_catalog_table.estimates.parameters["storage.location.template"] == format(
      "%s%s=$${%s}",
      one(aws_glue_catalog_table.estimates.storage_descriptor).location,
      one(aws_glue_catalog_table.estimates.partition_keys).name,
      one(aws_glue_catalog_table.estimates.partition_keys).name,
    )
    error_message = "the projection template must be the table's own location plus <partition key>=<substitution> — the publisher writes usage_date=<d>/ under the prefix, and a template addressing anything else projects partitions over empty locations"
  }

  assert {
    condition     = one(aws_glue_catalog_table.estimates.partition_keys).name == "usage_date"
    error_message = "the estimates table must partition on usage_date — it is the column the reconciliation view joins to the CUR leg's day, and the name the publisher writes into the object key"
  }

  # Open at the top, on a DAILY partition — so a closed range does not fail eventually,
  # it fails today: the current day falls outside the projection and the newest row in
  # the reconciliation is permanently missing.
  assert {
    condition = alltrue([
      aws_glue_catalog_table.estimates.parameters["projection.enabled"] == "true",
      endswith(aws_glue_catalog_table.estimates.parameters["projection.usage_date.range"], ",NOW"),
      aws_glue_catalog_table.estimates.parameters["projection.usage_date.type"] == "date",
      aws_glue_catalog_table.estimates.parameters["projection.usage_date.format"] == "yyyy-MM-dd",
      aws_glue_catalog_table.estimates.parameters["projection.usage_date.interval.unit"] == "DAYS",
    ])
    error_message = "the estimates table must project daily partitions in yyyy-MM-dd running to NOW — the publisher stamps %Y-%m-%d and writes one directory per day, and a closed range or a mismatched format leaves today's partition unqueryable with no error anywhere"
  }

  # The publisher writes newline-delimited JSON. Both JSON SerDes require exactly one
  # record per line, which is what it produces; any other SerDe reads the objects as
  # something they are not, and that one fails loudly rather than emptily.
  assert {
    condition = alltrue([
      one(one(aws_glue_catalog_table.estimates.storage_descriptor).ser_de_info).serialization_library == "org.openx.data.jsonserde.JsonSerDe",
      aws_glue_catalog_table.estimates.parameters["classification"] == "json",
    ])
    error_message = "the estimates table must read NDJSON through the OpenX JSON SerDe — that is the format the publisher writes, one json.dumps per line"
  }

  # The grant, scoped to the prefix the publisher is told to use. This is the single
  # most silent seam in the component: a PutObject the policy does not cover raises
  # inside _write_estimates, which logs a warning and returns, so the handler reports
  # status=ok on every batch, the CloudWatch metric path is untouched and healthy, and
  # the estimates table is simply empty forever.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.invocation_cost_publisher.policy).Statement :
      contains(s.Action, "s3:PutObject") && contains(
        tolist(s.Resource),
        "${aws_s3_bucket.estimates.arn}/${aws_lambda_function.invocation_cost_publisher.environment[0].variables["ESTIMATE_PREFIX"]}/*"
      )
    ])
    error_message = "the publisher's s3:PutObject grant must cover exactly the bucket and prefix it is configured to write to — an uncovered write fails inside a swallowed exception, so the export goes to zero while the Lambda keeps returning success and the reconciliation renders an empty estimate leg as agreement"
  }

  # And the retention rule has to be scoped to that same prefix, or it either never
  # expires the objects it was written for or expires things it was not.
  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.estimates.rule :
      r.id == "expire-estimates" && try(one(r.filter).prefix, null) == "${aws_lambda_function.invocation_cost_publisher.environment[0].variables["ESTIMATE_PREFIX"]}/"
    ])
    error_message = "the estimate retention rule must be scoped to the prefix the publisher writes to — pointed anywhere else it bounds nothing, and the bucket grows for the life of the account"
  }
}

# The view's estimate leg gets the same treatment its CUR leg already had: the columns
# are EXTRACTED from the query rather than restated, so a clause that reaches for a new
# column fails until the table declares it.
#
# The failure mode differs from the CUR leg's and is worse. A CUR column the table does
# not declare fails the query, which the reconciler records as unreadable spend. A
# missing ESTIMATE column does not fail anything: the JSON SerDe returns NULL for a
# column absent from the object, so SUM(estimate_usd) is NULL for every group and the
# view still emits a row per platform per day with a NULL where the number goes.
run "the_estimates_table_declares_every_column_the_view_reads" {
  command = plan

  # columns UNION partition keys. usage_date is a partition key rather than a column,
  # so a check against `columns` alone would fail on a correctly built table — the kind
  # of gate that gets deleted rather than fixed.
  assert {
    condition = alltrue([
      for col in distinct(flatten(regexall("e\\.([a-z_]+)", aws_athena_named_query.spend_reconciliation.query))) :
      contains(
        concat(
          [for c in one(aws_glue_catalog_table.estimates.storage_descriptor).columns : c.name],
          [for k in aws_glue_catalog_table.estimates.partition_keys : k.name],
        ),
        col
      )
    ])
    error_message = "the reconciliation view reads an estimate column the estimates table does not declare — the JSON SerDe returns NULL rather than failing, so the view still materializes and finance reads a row whose estimate is silently absent"
  }

  # The extraction is only a gate if it found something. regexall over a rewritten query
  # that stops aliasing the estimate leg returns [], and alltrue([]) is true.
  assert {
    condition     = length(distinct(flatten(regexall("e\\.([a-z_]+)", aws_athena_named_query.spend_reconciliation.query)))) >= 3
    error_message = "the reconciliation view no longer references the estimate leg through its alias — alltrue([]) is true, so the column check above passes vacuously the moment the extraction stops matching"
  }

  # The amount column with its type, the same way the CUR leg's is pinned. Declared as
  # a string, SUM() over it fails the query instead of reading zero.
  assert {
    condition = contains([
      for c in one(aws_glue_catalog_table.estimates.storage_descriptor).columns : c.name
      if c.type == "double"
    ], "estimate_usd")
    error_message = "estimate_usd must be declared as double — it is what the view SUMs, and summing a string column fails the query rather than reading zero"
  }

  # The join key, on both sides. platform_id carries the same value the publisher reads
  # off the invoking role's tag and the CUR leg recovers from the tags map; if it is not
  # a string the join is against something that is not comparable.
  assert {
    condition = contains([
      for c in one(aws_glue_catalog_table.estimates.storage_descriptor).columns : c.name
      if c.type == "string"
    ], "platform_id")
    error_message = "platform_id must be declared as string — it is the reconciliation's join key against the CUR leg's COALESCE over the tags map, and it is a data column rather than a partition so the view can aggregate across platforms without a per-partition predicate"
  }
}

# The cost-allocation tags are ASSERTED, not owned, and the assertion has to be able
# to fire in both directions.
#
# Terraform cannot own this. `aws_ce_tags` reads cost DATA dimensions, so a user-defined
# key shows up there only once it is already active — a gate on it waits for the thing
# it is trying to cause. Nothing can see an inactive key. And the provider's activation
# resource discards the per-key error the API returns inside a 200, so declaring it
# reports success while doing nothing.
#
# What is left is a verifier, and the same property that makes the data source useless
# as a gate makes it exact as one: presence IS activation.
run "an_unactivated_key_is_reported_loudly" {
  command = plan

  override_data {
    target = data.aws_ce_tags.observed
    values = { tags = ["CostCenter", "BusinessUnit"] }
  }

  # The check MUST fire. A check whose condition is always true is indistinguishable
  # from no check, so the expectation is what pins that this one can fail at all.
  expect_failures = [check.the_cost_allocation_tags_are_active]
}

# Half-activated is the interesting state, because it is the one that looks fine.
# PlatformId alone attributes every datastore and no model invocation, so the budget
# is confidently wrong in a specific direction rather than obviously broken — and the
# missing half is the dominant cost.
run "activating_only_the_resource_tag_is_still_a_failure" {
  command = plan

  override_data {
    target = data.aws_ce_tags.observed
    values = { tags = ["PlatformId", "CostCenter"] }
  }

  expect_failures = [check.the_cost_allocation_tags_are_active]
}

run "activating_only_the_principal_tag_is_still_a_failure" {
  command = plan

  override_data {
    target = data.aws_ce_tags.observed
    values = { tags = ["iamPrincipal/PlatformId"] }
  }

  expect_failures = [check.the_cost_allocation_tags_are_active]
}

# And it must go quiet when both are active, or it is a warning operators learn to
# scroll past — which is the same as not having it.
run "both_keys_active_is_silent" {
  command = plan

  override_data {
    target = data.aws_ce_tags.observed
    values = { tags = ["PlatformId", "iamPrincipal/PlatformId", "CostCenter"] }
  }

  assert {
    condition     = length(local.inactive_cost_allocation_tags) == 0
    error_message = "with both keys active the check must not fire — a gate that cries wolf on a correctly configured account gets ignored on the run that matters"
  }
}

# The pipeline stamps its own storage regardless, because that observation is what puts
# the key into AWS's inventory in the first place. Without it there is nothing for a
# human to activate, and the account waits forever for a key that will never be listed.
run "the_pipeline_seeds_the_key_it_asks_to_have_activated" {
  command = plan

  override_data {
    target = data.aws_ce_tags.observed
    values = { tags = [] }
  }

  expect_failures = [check.the_cost_allocation_tags_are_active]

  assert {
    condition     = aws_s3_bucket.estimates.tags["PlatformId"] == "org"
    error_message = "the pipeline must tag its own storage with PlatformId even while the key is unactivatable — that observation is what makes AWS list the key at all"
  }
}
