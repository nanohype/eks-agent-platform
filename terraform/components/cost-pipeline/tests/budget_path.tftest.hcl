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

# The operator executes the Athena query, and Athena reaches S3 and KMS under the
# CALLER's identity. Reading the SSE-KMS estimates and writing the SSE-KMS result set
# are both on this key, and the workgroup enforces encrypted results — so Decrypt
# alone leaves every query failing at the write step.
run "the_operator_can_decrypt_and_write_what_it_queries" {
  command = plan

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
      contains(s.Action, "kms:Decrypt")
      && contains(s.Action, "kms:GenerateDataKey")
      && contains(tolist(s.Resource), var.data_kms_key_arn)
    ])
    error_message = "the operator role must hold kms:Decrypt AND kms:GenerateDataKey on the cost data key — without both, StartQueryExecution returns FAILED on every tick, reconcileBudget returns before the kill-switch block, and a tenant at 120% of budget never trips it with nothing going red"
  }

  # The grant is capped to S3, so it cannot be used to decrypt anything else the key
  # protects. A grant that works and reaches further than it needs is the next finding.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
      !contains(s.Action, "kms:Decrypt") || try(s.Condition.StringEquals["kms:ViaService"], null) != null
    ])
    error_message = "the KMS grant must be conditioned on kms:ViaService so it only works through S3"
  }
}

# AWS's Cost and Usage Reports service delivers with the two S3 statements it
# publishes and nothing else — it holds no KMS grant, and AWS documents none. An
# SSE-KMS default on this bucket does not harden it, it makes every delivery fail, so
# the bucket stays empty and every budget reads zero.
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
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.invocation_cost_publisher.policy).Statement :
      !contains(s.Action, "iam:ListRoleTags") || alltrue([for r in tolist(s.Resource) : !endswith(r, ":role/*")])
    ])
    error_message = "the tag-read grant must be scoped to the operator's IAM path, not to every role in the account"
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
# The suite's global mock returns an ARN for every aws_ssm_parameter, which is fine for the
# resources that only need A value but makes any assertion about THIS one vacuous — it
# happens not to end in a slash and happens not to end in ":role/*", so both the
# normalization and the scoping assertions would pass without testing anything. These runs
# override the data source with the two shapes that actually matter.
run "the_tag_read_grant_survives_a_path_without_a_trailing_slash" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.tenant_iam_path
    values = { value = "/eks-agent-platform/tenants" }
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

  override_data {
    target = data.aws_ssm_parameter.tenant_iam_path
    values = { value = "/eks-agent-platform/tenants/" }
  }

  # Normalization must be idempotent — a double slash is a different path to IAM, and
  # would miss just as completely as a missing one.
  assert {
    condition     = local.tenant_iam_path == "/eks-agent-platform/tenants/"
    error_message = "normalizing an already-normalized path must not append a second slash"
  }
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
    error_message = "the workgroup must enforce SSE_KMS results on the same key the operator policy grants — enforcing a key the caller cannot use fails every query at the write step"
  }
}
