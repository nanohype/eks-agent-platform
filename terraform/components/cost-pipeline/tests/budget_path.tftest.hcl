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

  # The activation resource is count-gated on whether Cost Explorer has observed the
  # key, so without a mock the count is unknown at plan and the whole suite errors
  # before any assertion runs. Default to observed; the run that cares about the
  # other state overrides it.
  mock_data "aws_ce_tags" {
    defaults = {
      tags = ["PlatformId", "CostCenter", "BusinessUnit"]
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

# The operator executes the Athena query, and Athena reaches S3 and KMS under the
# CALLER's identity. Reading the SSE-KMS estimates and writing the SSE-KMS result set
# are both on this key, and the workgroup enforces encrypted results — so Decrypt
# alone leaves every query failing at the write step.
run "aws_can_actually_deliver_the_report" {
  command = plan

  assert {
    condition = alltrue([
      for r in aws_s3_bucket_server_side_encryption_configuration.cur.rule :
      alltrue([
        for d in r.apply_server_side_encryption_by_default :
        d.sse_algorithm == "AES256"
      ])
    ])
    error_message = "the CUR bucket must default to SSE-S3 — billingreports.amazonaws.com holds no KMS grant and AWS publishes none, so an SSE-KMS default silently prevents every delivery"
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
    condition     = local.cur_platform_tag_column == "resource_tags_user_platform_id"
    error_message = "AWS inserts an underscore before each uppercase letter, so resourceTags/user:PlatformId becomes resource_tags_user_platform_id — the split inside PlatformId is the part a hand-written name gets wrong, and a wrong column makes the query valid, green and empty"
  }

  # The reconciliation view is created by this saved query, so its SQL is the finance-facing
  # consumer. It read a different column from the reconciler, so one of the two was always
  # returning nothing.
  assert {
    condition     = strcontains(aws_athena_named_query.spend_reconciliation.query, local.cur_platform_tag_column)
    error_message = "the reconciliation view must filter on the same column the reconciler does"
  }

  assert {
    condition     = !strcontains(aws_athena_named_query.spend_reconciliation.query, "resource_tags_user_platformid")
    error_message = "the reconciliation view still carries the pre-fix column spelling — a view that reads zero while the reconciler reads correctly is worse than both being wrong, because the two disagree silently"
  }
}

# NOTE on the us-east-1 providers. `aws_cur_report_definition` and
# `aws_ce_cost_allocation_tag` reach APIs with no endpoint outside us-east-1, and both are
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

# The cost-allocation tag activates itself on a later apply, and that is a design
# rather than an omission.
#
# AWS only offers a user-defined key for activation once it has OBSERVED a resource
# carrying it, then takes up to 24h to list it. So activation cannot happen in the
# apply that first stamps the key — and on a fresh account nothing has stamped it,
# because the tenants that would carry it do not exist until this pipeline does. A
# blocking precondition would deadlock that permanently.
#
# Gating the RESOURCE on the observation makes it a two-act contract that completes
# itself: act one builds the pipeline and stamps the key on its own buckets, act two
# activates it once AWS has caught up. Both states are asserted, because "it applies
# on a fresh account" and "it eventually activates" are different claims and only
# testing one of them is how this deadlocked in an earlier draft.
run "a_fresh_account_still_plans_before_the_key_is_observable" {
  command = plan

  override_data {
    target = data.aws_ce_tags.observed
    values = { tags = ["CostCenter", "BusinessUnit"] }
  }

  # The check block MUST fire here. It is the loud half of the contract: while the
  # key is unobservable every plan says so, rather than the pipeline quietly building
  # itself without the column its consumers filter on. Asserting the warning is
  # expected also pins that it can fire at all — a check whose condition is always
  # true is indistinguishable from no check.
  expect_failures = [check.platform_id_tag_is_active]

  assert {
    condition     = length(aws_ce_cost_allocation_tag.platform_id) == 0
    error_message = "with the key not yet observed the activation must not be attempted — a fresh account has nothing tagged PlatformId, so a component that insists on activating it can never apply, and the pipeline that would create the first tagged resource never gets built"
  }

  # And the pipeline still stamps the key, which is what starts AWS's clock. Without
  # this the two-act contract has no act one and the account waits forever.
  assert {
    condition     = aws_s3_bucket.cur.tags["PlatformId"] == "org"
    error_message = "the pipeline must tag its own storage with PlatformId even before the key is activatable — that observation is what makes AWS list the key at all"
  }
}

run "the_activation_appears_once_the_key_is_observed" {
  command = plan

  override_data {
    target = data.aws_ce_tags.observed
    values = { tags = ["PlatformId", "CostCenter"] }
  }

  assert {
    condition     = length(aws_ce_cost_allocation_tag.platform_id) == 1
    error_message = "once Cost Explorer lists the key the activation must be created — otherwise resource_tags_user_platform_id is never a CUR column and every tenant's CUR spend reads zero through a query that succeeds"
  }

  assert {
    condition     = aws_ce_cost_allocation_tag.platform_id[0].status == "Active"
    error_message = "the tag must be activated, not merely declared"
  }

  # Case matters: Cost Explorer treats tag keys as case-sensitive and the CUR column
  # is derived from the key, so a lowercase spelling activates a different tag and
  # leaves the reconciler's column absent.
  assert {
    condition     = aws_ce_cost_allocation_tag.platform_id[0].tag_key == "PlatformId"
    error_message = "the activated key must be PlatformId exactly — the operator derives resource_tags_user_platform_id from this spelling"
  }
}
