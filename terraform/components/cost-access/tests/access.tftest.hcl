# One cluster's access to the account cost pipeline.
#
# The grant assertions moved here with the grant itself, because they are per-cluster
# claims: the pipeline is applied once for the account, and N clusters need N
# policies attached to N operator roles. What is asserted is the far end of the path
# — that the operator can actually run the query the reconciler issues, and that the
# handles it sweeps resolve to the account's pipeline rather than to nothing.

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
  # Every SSM read resolves to this unless a run overrides it. Enough to plan; the
  # runs that care about a SPECIFIC value override that parameter, because a suite
  # where every handle is the same string cannot tell a correct wiring from a
  # transposed one.
  mock_data "aws_ssm_parameter" {
    defaults = {
      value = "/eks-agent-platform/tenants/"
    }
  }
  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::123456789012:policy/mock-policy"
    }
  }
}

# The one account handle that cannot take the shared default. The provider validates a
# workgroup's kms_key_arn as an ARN client-side, so the fixture's IAM-path default fails
# the plan outright rather than any assertion. Runs that assert on the key override this
# with their own sentinel; a run-level override wins.
override_data {
  target = data.aws_ssm_parameter.account_data_kms_key_arn
  values = { value = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000" }
}

variables {
  environment  = "staging"
  region       = "us-west-2"
  cluster_name = "staging-platform"
  tags         = {}
}

# Athena reaches S3 and KMS under the CALLER's identity, and the caller is the
# operator role this policy attaches to. Reading the SSE-KMS estimates and writing
# the SSE-KMS result set are both on this key, and the workgroup enforces encrypted
# results — so Decrypt alone leaves every query failing at the write step, the
# reconciler returns before the kill-switch block, and a tenant at 120% of budget
# never trips it with nothing going red.
run "the_operator_can_decrypt_and_write_what_it_queries" {
  command = plan

  # A sentinel, not the fixture default. The key the grant names has to come from the
  # account contract — the pipeline is the only thing that decides which key encrypts
  # the results — and a suite that asserted a literal ARN would pass just as happily
  # if this component invented its own value, which is the whole defect.
  override_data {
    target = data.aws_ssm_parameter.account_data_kms_key_arn
    values = { value = "arn:aws:kms:us-west-2:123456789012:key/SENTINEL-ACCOUNT-COST-KEY" }
  }

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
      contains(s.Action, "kms:Decrypt")
      && contains(s.Action, "kms:GenerateDataKey")
      && contains(tolist(s.Resource), "arn:aws:kms:us-west-2:123456789012:key/SENTINEL-ACCOUNT-COST-KEY")
    ])
    error_message = "the operator role must hold kms:Decrypt AND kms:GenerateDataKey on the key the ACCOUNT pipeline published — without both, StartQueryExecution returns FAILED on every tick; and named against any other key the failure lands on the SSE-KMS write, which the reconciler reports as unreadable spend rather than as access denied, so a tenant over budget never trips the kill switch"
  }

  # And the read is from the right contract path. The override above targets the data
  # source by LABEL, so it substitutes a value no matter which parameter that source
  # actually names — the assertion above passes even if this reads a different key
  # entirely. Proven by mutation: repointing it at the account's athena_database stayed
  # green, and that path resolves, so the apply succeeds and scopes the operator's KMS
  # grant to a Glue database name.
  assert {
    condition     = data.aws_ssm_parameter.account_data_kms_key_arn.name == "/eks-agent-platform/org/cost-pipeline/data_kms_key_arn"
    error_message = "the cost key must be read from the account contract's data_kms_key_arn — every other key under that prefix resolves too, so a near-miss applies cleanly and scopes the operator's KMS grant to something that is not a key"
  }

  # And the workgroup enforces that same key. The operator sends no ResultConfiguration,
  # so the workgroup is the only thing deciding how results are encrypted — a grant on
  # one key and enforcement on another fails at the write, after the scan.
  assert {
    condition     = one(one(aws_athena_workgroup.cluster.configuration).result_configuration).encryption_configuration[0].kms_key_arn == "arn:aws:kms:us-west-2:123456789012:key/SENTINEL-ACCOUNT-COST-KEY"
    error_message = "this cluster's workgroup must enforce the same account key the operator's grant names — enforcement on a key the caller cannot GenerateDataKey with fails every query at the SSE-KMS write, which the reconciler reports as unreadable spend rather than as access denied"
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

# Where this cluster writes, and where it is permitted to write, are one claim.
#
# The workgroup's output location and the object grant's resource are both derived from
# var.cluster_name, and the assertions derive them the same way rather than repeating a
# literal — two hardcoded strings that agree prove nothing about the pair, which is the
# defect the KMS run above was written to correct.
#
# A workgroup writing where the policy does not permit fails on the WRITE, after the
# scan: the reconciler reports unreadable spend rather than access denied, returns
# before the kill-switch block, and every budget holds its last value with nothing red.
run "this_cluster_writes_only_where_it_is_permitted_to_write" {
  command = plan

  # The name and the ARN are separate account handles, and both have to denote the same
  # bucket: the workgroup composes an s3:// URI from the name, the grant is scoped to the
  # ARN. Overridden to matching sentinels so an assertion cannot pass on the fixture
  # default resolving both to the same string.
  override_data {
    target = data.aws_ssm_parameter.athena_results_bucket
    values = { value = "SENTINEL-results-bucket" }
  }
  override_data {
    target = data.aws_ssm_parameter.athena_results_bucket_arn
    values = { value = "arn:aws:s3:::SENTINEL-results-bucket" }
  }

  assert {
    condition     = one(one(aws_athena_workgroup.cluster.configuration).result_configuration).output_location == "s3://SENTINEL-results-bucket/results/${var.cluster_name}/"
    error_message = "the workgroup must write under this cluster's own prefix in the account results bucket — a shared prefix is what lets one cluster's operator overwrite the objects another cluster's reconciler reads back"
  }

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
      contains(s.Action, "s3:PutObject")
      # length + contains, not an equality against a literal list: tolist() yields
      # list(string) and the literal is a tuple, so `==` is false for identical
      # contents. "Exactly one resource, and it is this one" is the same claim.
      && length(tolist(s.Resource)) == 1
      && contains(tolist(s.Resource), "arn:aws:s3:::SENTINEL-results-bucket/results/${var.cluster_name}/*")
    ])
    error_message = "the object grant must cover exactly this cluster's results prefix and nothing else — wider and a compromised operator overwrites another cluster's results, narrower or elsewhere and every query fails at the write with the reconciler reporting unreadable spend"
  }

  # ListBucket is a BUCKET-level action. On an object ARN it matches nothing, and Athena
  # fails at listing before it writes anything — so the split has to survive, not just
  # the narrowing.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
      contains(s.Action, "s3:ListBucket")
      && contains(tolist(s.Resource), "arn:aws:s3:::SENTINEL-results-bucket")
    ])
    error_message = "s3:ListBucket must be granted on the bucket ARN, not an object ARN — collapsing it into the object prefix statement matches nothing and every query fails at listing"
  }

  # The workgroup the operator is handed is the one its policy covers. These ship
  # together or the operator gets a name it may not use.
  assert {
    condition = alltrue([
      nonsensitive(aws_ssm_parameter.athena_workgroup.value) == aws_athena_workgroup.cluster.name,
      anytrue([
        for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
        contains(s.Action, "athena:StartQueryExecution")
        # flatten, not tolist: this statement's Resource is a single string, and IAM
        # accepts either form. tolist() on a string raises rather than failing the
        # assertion, which would make the run error instead of reporting the defect.
        && contains(flatten([s.Resource]), aws_athena_workgroup.cluster.arn)
      ]),
    ])
    error_message = "the republished workgroup handle and the athena grant must name the same workgroup — the operator runs in whatever the handle says, so a grant on any other workgroup is AccessDenied on every tick and the kill switch never fires"
  }

  # And the configuration is ENFORCED, which is what makes the prefix above a boundary
  # rather than a default. Without it a caller's own ResultConfiguration wins, and the
  # caller chooses both the output location and the encryption — so the per-cluster
  # prefix becomes advisory and the SSE-KMS guarantee goes with it. Nothing else in this
  # suite can see that: every assertion here reads the workgroup's own configuration,
  # which stays exactly as written while quietly ceasing to apply.
  assert {
    condition     = one(aws_athena_workgroup.cluster.configuration).enforce_workgroup_configuration
    error_message = "the workgroup must enforce its configuration — unenforced, a caller supplies its own output location and encryption, so this cluster's results prefix stops being a boundary and the SSE-KMS requirement stops being a requirement, with the workgroup still reading as correct"
  }

  # The name has to satisfy the operator's own identifier check, or it refuses to build
  # the query at all — a different failure from AccessDenied, and just as quiet.
  assert {
    condition     = can(regex("^[a-zA-Z0-9_-]{1,128}$", aws_athena_workgroup.cluster.name))
    error_message = "the workgroup name must match the operator's athenaIdentifierRE — it validates the name before building a query and returns early if it fails, so an unmatched name stops every budget tick before Athena is ever called"
  }
}

# The CUR read used to come from the bucket's own policy, naming one cluster's
# operator role. A bucket serving every cluster cannot do that, so it moved into the
# identity policy — and if it moved and did not land, the reconciler's Athena scan of
# the CUR fails and every tenant's CUR spend reads unreadable.
run "the_operator_can_read_the_account_cur" {
  command = plan

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
      s.Sid == "CurRead" && contains(s.Action, "s3:GetObject") && contains(s.Action, "s3:ListBucket")
    ])
    error_message = "the operator must be granted s3:GetObject and s3:ListBucket on the account CUR bucket through its identity policy — the bucket policy no longer names it, so nothing else grants this"
  }
}

# The whole point of the split: the operator only ever reads its own cluster's SSM
# subtree, so the account's handles have to arrive under that prefix or the budget
# reconciler degrades to errAthenaNotConfigured on every tick.
run "the_handles_land_where_the_operator_sweeps" {
  command = plan

  assert {
    condition = alltrue([
      for p in [
        aws_ssm_parameter.athena_workgroup,
        aws_ssm_parameter.athena_database,
        aws_ssm_parameter.cur_table_name,
      ] : startswith(p.name, "/eks-agent-platform/${var.cluster_name}/cost-pipeline/")
    ])
    error_message = "every republished handle must sit under this cluster's prefix — the operator's entire configuration is one GetParametersByPath sweep of that subtree, so a parameter published anywhere else is invisible to it"
  }

  # Exactly these three, by their full names. They are the whole of what the Budget
  # reconciler needs: it refuses to build a query with any of the workgroup, the
  # database or the CUR table empty, and it needs nothing else from this pipeline.
  #
  # The last segment is the part that matters — the operator decodes each key by that
  # exact string, so a near-miss publishes and sweeps cleanly and decodes to an empty
  # field, which reads as an unconfigured cost pipeline rather than as a typo.
  assert {
    condition = alltrue([
      aws_ssm_parameter.athena_workgroup.name == "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_workgroup",
      aws_ssm_parameter.athena_database.name == "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_database",
      aws_ssm_parameter.cur_table_name.name == "/eks-agent-platform/${var.cluster_name}/cost-pipeline/cur_table_name",
    ])
    error_message = "the three republished keys must carry exactly the names the operator decodes — the reconciler refuses to build a query with any of workgroup, database or CUR table empty, so a near-miss on any one of them degrades every tick to errAthenaNotConfigured with nothing going red"
  }
}

# The cross-tier check. The account pipeline scopes its publisher's tag-read grant to
# one IAM path and cannot verify it, because agent-iam publishes that path per
# cluster and the account has no cluster subtree to read. This component sees both.
run "a_pipeline_that_cannot_read_this_clusters_roles_is_refused" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.account_tenant_iam_path
    values = { value = "/eks-agent-platform/somewhere-else/" }
  }

  expect_failures = [aws_iam_policy.operator_cost]
}

# And the same check must not cry wolf. The path is a prefix, so a trailing slash on
# one side and not the other is the same path to a reader and a different one to a
# naive string compare — a gate that fails on that gets bypassed, and then it is not
# a gate.
run "a_trailing_slash_difference_is_not_drift" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.account_tenant_iam_path
    values = { value = "/eks-agent-platform/tenants" }
  }

  assert {
    condition     = local.normalized_account_path == local.normalized_cluster_path
    error_message = "a missing trailing slash must normalize rather than register as drift — both sides use the value as a prefix, so they mean the same thing"
  }
}

# A policy attached to nothing is a policy that grants nothing.
#
# Deleting the attachment outright used to leave this suite passing: every run asserted
# the policy's CONTENTS and none asserted that anything carried it. The operator would
# hold no cost access at all, the reconciler would fail every Athena call on
# AccessDenied, and the plan would look identical.
run "the_policy_is_attached_to_the_role_that_needs_it" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.operator_role_name
    values = { value = "SENTINEL-operator-role" }
  }

  assert {
    condition     = nonsensitive(aws_iam_role_policy_attachment.operator_cost.role) == "SENTINEL-operator-role"
    error_message = "the cost policy must attach to the operator role agent-iam published — an unattached policy grants nothing, and every assertion about its contents holds just as well"
  }

  assert {
    condition     = aws_iam_role_policy_attachment.operator_cost.policy_arn == aws_iam_policy.operator_cost.arn
    error_message = "the attachment must carry THIS component's policy, not some other ARN"
  }
}

# Every handle carries its own value, and the suite can tell them apart.
#
# The mock provider gives every aws_ssm_parameter read the same default, so a suite that
# asserts only on parameter NAMES passes identically when the values behind them are
# transposed — the database republished from the table-name handle, or the reverse. The
# operator sweeps the right keys and decodes the wrong values, and every query it builds
# is syntactically fine and points at nothing.
#
# So each source gets a distinct sentinel and each assertion names the one it expects.
#
# The workgroup handle is not here: it no longer carries an account value at all. It
# carries this cluster's own workgroup, and it is asserted against the grant that must
# cover it in the run above, which is the pairing that actually matters for it.
run "each_account_handle_republishes_its_own_value" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.athena_database
    values = { value = "SENTINEL-database" }
  }
  override_data {
    target = data.aws_ssm_parameter.cur_table_name
    values = { value = "SENTINEL-cur-table" }
  }

  assert {
    condition = alltrue([
      nonsensitive(aws_ssm_parameter.athena_database.value) == "SENTINEL-database",
      nonsensitive(aws_ssm_parameter.cur_table_name.value) == "SENTINEL-cur-table",
    ])
    error_message = "every republished handle must carry the value of the account handle it names — a transposition publishes under the right key, sweeps cleanly, and hands the operator a query that points at the wrong object"
  }
}
