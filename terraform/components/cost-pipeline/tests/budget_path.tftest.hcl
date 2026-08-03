# The budget path, asserted end to end at plan time.
#
# The kill switch is the product's central promise, and it had four independent
# breaks on one path — the report definition created through a region whose API does
# not exist, a CUR bucket AWS cannot write, an operator with no KMS on the key that
# encrypts what it reads and writes, and a cost-allocation tag that was never
# activated so the column the query filters on was never in the data. Any three fixed
# left it dead. Each of those is a separate assertion here, because that is the shape
# the defect took: every piece valid, the pair broken, and nothing red anywhere.

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
  region           = "us-west-2"
  data_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111"
  tags             = {}
}

# The operator executes the Athena query, and Athena reaches S3 and KMS under the
# CALLER's identity. Reading the SSE-KMS estimates and writing the SSE-KMS result set
# are both on this key, and the workgroup enforces encrypted results — so Decrypt
# alone leaves every query failing at the write step.
run "aws_can_actually_deliver_the_report" {
  command = plan

  assert {
    condition = alltrue([
      for r in aws_s3_bucket_server_side_encryption_configuration.estimates.rule :
      alltrue([
        for d in r.apply_server_side_encryption_by_default :
        d.sse_algorithm == "AES256"
      ])
    ])
    error_message = "the estimates bucket must carry a default encryption rule — the publisher passes its own SSE-KMS key per object, but a bucket with no default leaves anything written by another path unencrypted at rest"
  }

  # The estimates the cost publisher writes keep the CMK: the Lambda sets the header
  # per object, which overrides the bucket default. Losing that is a real downgrade,
  # so the grant that reads them back is asserted above.
  assert {
    condition     = contains(keys(aws_lambda_function.invocation_cost_publisher.environment[0].variables), "ESTIMATE_KMS_KEY_ID")
    error_message = "the cost publisher must still be handed a CMK for its own writes — the bucket default only governs what AWS delivers"
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
    error_message = "AWS inserts an underscore before each uppercase letter, so resourceTags/user:PlatformId becomes resource_tags_user_platform_id — the split inside PlatformId is the part a hand-written name gets wrong, and a wrong column makes the query valid, green and empty"
  }

  # The reconciliation view is created by this saved query, so its SQL is the finance-facing
  # consumer. It read a different column from the reconciler, so one of the two was always
  # returning nothing.
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

  assert {
    condition     = !strcontains(aws_athena_named_query.spend_reconciliation.query, "resource_tags_user_platformid")
    error_message = "the reconciliation view still carries the pre-fix column spelling — a view that reads zero while the reconciler reads correctly is worse than both being wrong, because the two disagree silently"
  }
}

# NOTE on the us-east-1 provider. Cost Explorer reaches APIs with no endpoint outside us-east-1, and both are
# created through `aws.us_east_1`. That binding is NOT assertable here — the `provider` meta
# argument is not a resource attribute, so a run block cannot see it, and an assertion phrased
# around it would pass with the argument deleted. What actually binds it is
# `configuration_aliases = [aws.us_east_1]` in versions.tf: a caller that does not pass the
# alias fails at init, before any assertion runs. Said out loud because the obvious-looking
# assertion is the vacuous one.

# The workgroup enforces encrypted results on the same key the operator is granted
# above. Asserted as a relation so the pair cannot drift apart: an enforced key the
# operator cannot use is the exact defect this suite exists to prevent.
run "the_workgroup_enforces_the_key_the_operator_holds" {
  command = plan

  assert {
    condition = alltrue([
      for c in aws_athena_workgroup.cost.configuration :
      c.enforce_workgroup_configuration
      && alltrue([
        for rc in c.result_configuration :
        alltrue([
          for e in rc.encryption_configuration :
          e.encryption_option == "SSE_KMS" && e.kms_key_arn == var.data_kms_key_arn
        ])
      ])
    ])
    error_message = "the workgroup must enforce SSE_KMS results on data_kms_key_arn. The other half of this pair — that each cluster's operator actually holds Decrypt AND GenerateDataKey on that key — is asserted in cost-access, which is where the grant is minted; enforcing a key the caller cannot use fails every query at the write step"
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
