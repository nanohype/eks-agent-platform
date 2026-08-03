# The account+region singleton, asserted where it is decided.
#
# This component exists because `aws_bedrock_model_invocation_logging_configuration`
# has no name and no identifier: AWS keeps exactly one per account per region. Owned
# per environment, the last apply silently repoints the account's logging at that
# environment's bucket and ANY environment's destroy deletes it for all of them —
# both green, nothing red, and the invocation record is what every budget decision
# reads.
#
# So the assertions here are about SINGULARITY, not about wiring: that nothing this
# component names can carry a cluster or environment token, and that the one
# configuration points at the buckets this component owns rather than at whatever
# was there before.

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
  # The provider validates this one client-side: the logging configuration parses
  # role_arn before it ever reaches AWS, so a generated placeholder fails the plan.
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock-bedrock-logging"
    }
  }
}

variables {
  logs_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/11111111-1111-1111-1111-111111111111"
  tags             = {}
}

# A name carrying an environment or a cluster is a claim this component cannot keep:
# there is one of these per account, so a name implying otherwise tells the next
# reader it is safe to have a second.
run "nothing_is_named_for_an_environment_or_a_cluster" {
  command = plan

  assert {
    condition     = startswith(aws_s3_bucket.invocations.bucket, "org-123456789012-us-west-2-")
    error_message = "the invocations bucket must be named for the account and region only — a cluster or environment token would imply this resource can exist more than once, and it cannot"
  }

  assert {
    condition = alltrue([
      for name in [aws_s3_bucket.invocations.bucket, aws_s3_bucket.access_logs.bucket, aws_cloudwatch_log_group.invocations.name] :
      !can(regex("development|staging|production", name))
    ])
    error_message = "an account-scoped resource must not carry a workload environment token in its name"
  }

  # The region IS in the name, deliberately. S3's namespace is global while Bedrock
  # logging is per-region, so two regions in one account need two buckets and the
  # names must differ.
  assert {
    condition     = strcontains(aws_s3_bucket.invocations.bucket, "us-west-2")
    error_message = "the region must appear in the bucket name — the logging configuration is per-region and S3's namespace is global, so a second region would otherwise collide on the name"
  }
}

# The singleton points at what this component owns. Asserted as a relation rather
# than against a literal: a literal would keep passing if the buckets were renamed
# and the configuration left aimed at the old ones.
#
# Two things make a relation like this weaker than it looks, and both are guarded here:
#
#   `alltrue([])` is TRUE. Every one of these blocks is optional to the provider, so a
#   loop over an absent one asserts nothing — deleting `s3_config`, or the whole
#   `logging_config`, leaves Bedrock delivering nowhere and the run green. The block
#   existing is half the claim, so length() carries that half.
#
#   Two buckets of the same type have the same computed `id` under a mock provider.
#   Comparing against `.id` therefore cannot tell the invocation record apart from its
#   own access log, and aiming the configuration at the wrong one passes. The resource
#   references `.bucket` — the configured name — so these compare distinct values.
run "the_one_configuration_points_at_the_buckets_this_component_owns" {
  command = plan

  assert {
    condition = length(aws_bedrock_model_invocation_logging_configuration.this.logging_config) > 0 && alltrue([
      for c in aws_bedrock_model_invocation_logging_configuration.this.logging_config :
      length(c.s3_config) > 0 && alltrue([
        for s in c.s3_config : s.bucket_name == aws_s3_bucket.invocations.bucket
      ])
    ])
    error_message = "the invocation logging configuration must carry an s3_config delivering to this component's own bucket — there is one configuration per account, so pointing it anywhere else silently takes over or abandons the account's audit trail, and omitting the block entirely delivers nowhere"
  }

  assert {
    condition = length(aws_bedrock_model_invocation_logging_configuration.this.logging_config) > 0 && alltrue([
      for c in aws_bedrock_model_invocation_logging_configuration.this.logging_config :
      length(c.cloudwatch_config) > 0 && alltrue([
        for l in c.cloudwatch_config : l.log_group_name == aws_cloudwatch_log_group.invocations.name
      ])
    ])
    error_message = "the configuration must carry a cloudwatch_config naming this component's log group — the cost publisher subscribes to it through the SSM contract, so a mismatch or an absent block leaves the subscription attached to a group nothing writes to"
  }

  # All four modalities. Each flag defaults to false, so dropping one narrows the
  # account's audit trail to a subset of its own traffic while every apply stays
  # green — and the budget reads the resulting metric as though it were the whole.
  assert {
    condition = length(aws_bedrock_model_invocation_logging_configuration.this.logging_config) > 0 && alltrue([
      for c in aws_bedrock_model_invocation_logging_configuration.this.logging_config :
      c.text_data_delivery_enabled
      && c.image_data_delivery_enabled
      && c.embedding_data_delivery_enabled
      && c.video_data_delivery_enabled
    ])
    error_message = "every data-delivery modality must be enabled — these default to false, so a dropped flag silently removes a class of invocation from the account's only audit trail and from everything derived from it"
  }
}

# The access-log pairing, asserted on the configured names for the same reason as
# above: under mocks both buckets' computed ids are the same value, so a bucket
# logging to ITSELF satisfies an `.id` comparison. Self-logging a versioned WORM
# bucket is a recursion AWS accepts and bills for.
run "the_invocation_record_logs_to_the_access_log_bucket" {
  command = plan

  assert {
    condition     = aws_s3_bucket_logging.invocations.target_bucket == aws_s3_bucket.access_logs.bucket
    error_message = "server-access logs from the invocations bucket must land in the access-logs bucket — pointing this at the invocations bucket itself makes the audit trail log its own reads back into itself"
  }

  assert {
    condition     = aws_s3_bucket_logging.invocations.target_bucket != aws_s3_bucket.invocations.bucket
    error_message = "the invocations bucket must not be its own access-log destination"
  }
}

# The SSM contract is the join to cost-pipeline, which is a separate root. A
# terragrunt dependency across roots would make every leaf's PARSE depend on this
# state existing, so the handoff is a published parameter instead — and it has to
# actually carry the log group the configuration writes to.
run "the_published_contract_matches_what_is_configured" {
  command = plan

  assert {
    condition     = aws_ssm_parameter.invocation_log_group.value == aws_cloudwatch_log_group.invocations.name
    error_message = "the published log-group name must be the one the logging configuration writes to — cost-pipeline attaches its subscription filter to this value, and a stale one attaches it to nothing"
  }

  assert {
    condition     = startswith(aws_ssm_parameter.invocation_log_group.name, "/eks-agent-platform/org/")
    error_message = "account-scoped parameters must live under the reserved `org` segment; a cluster-shaped path would collide with the subtree the operator sweeps"
  }
}

# Teardown posture. This component carries no force_destroy_buckets lever, because
# there is no environment here that could make an account's audit trail disposable —
# what governs deletion is the Object Lock mode, and that is a retention decision.
run "compliance_mode_blocks_the_destroy_it_promises_to_block" {
  command = plan

  variables {
    object_lock_mode = "COMPLIANCE"
  }

  assert {
    condition     = aws_s3_bucket.invocations.force_destroy == false
    error_message = "under COMPLIANCE nothing can shorten the retention, including root, so force_destroy would be a promise the bucket cannot keep — it must be false"
  }

  # The access-logs bucket carries no Object Lock, so it COULD be emptied under
  # COMPLIANCE — and it deliberately is not. The two buckets are one audit artifact:
  # emptying the record of who read the invocation log while the invocation log itself
  # is legally immutable produces a WORM bucket nobody can account for. Both sides of
  # this are asserted because the mode governed only one of them before, and an
  # untested branch of a two-branch rule is the branch that drifts.
  assert {
    condition     = aws_s3_bucket.access_logs.force_destroy == false
    error_message = "under COMPLIANCE the access-logs bucket must be protected alongside the invocation record — they are one audit artifact, and emptying the access history of an immutable log leaves a record nobody can account for"
  }
}

run "governance_mode_permits_the_destroy_it_promises_to_permit" {
  command = plan

  variables {
    object_lock_mode = "GOVERNANCE"
  }

  assert {
    condition     = aws_s3_bucket.invocations.force_destroy
    error_message = "under GOVERNANCE a caller holding s3:BypassGovernanceRetention can clear the lock, so the bucket must permit the destroy rather than wedge a teardown that is allowed to proceed"
  }

  # The access-logs bucket takes writes from the invocations bucket's first PUT, so
  # it is non-empty long before anyone tries to tear anything down. A gate on one
  # bucket and not the other empties half the component and then wedges.
  assert {
    condition     = aws_s3_bucket.access_logs.force_destroy
    error_message = "the access-logs bucket must move on the same lever as the invocations bucket — AWS spreads these gates across resources the dependency graph does not join, so a partially-gated component destroys what it can and then blocks"
  }
}

# The invocations bucket is versioned and WORM. Object Lock holds every version until
# its own retain-until date, so nothing bounds it early — but transitions alone move
# the cost down a tier and never end it, which leaves the account's invocation record
# growing for the life of the account and a teardown with nothing to reach. The expiry
# has to outlast the lock, or it is a plan AWS refuses object by object.
#
# This matters more here than it did per-environment: there is now exactly one of
# these buckets and it carries every environment's invocations, so an unbounded record
# is the account's problem rather than one environment's.
run "the_invocation_record_is_bounded_once_the_lock_lapses" {
  command = plan

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.invocations.rule :
      r.status == "Enabled"
      && anytrue([for e in r.expiration : e.days > var.object_lock_retention_days])
      && length(r.noncurrent_version_expiration) > 0
    ])
    error_message = "the invocations bucket needs an expiry that outlasts object_lock_retention_days, plus a noncurrent-version sweep — transitions bound the cost tier, not the object count"
  }

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.invocations.rule :
      r.status == "Enabled" && anytrue([
        for e in r.expiration :
        e.expired_object_delete_marker == true && e.days == 0 && e.date == null
      ])
    ])
    error_message = "the invocations bucket needs a delete-marker sweep in a rule of its own — on a versioned bucket an expiry writes a marker that is itself a current version, so without this the bucket only grows and a teardown never ends"
  }
}

# The access-logs bucket is bounded by its lifecycle rule and nothing else: it takes
# no Object Lock and its whole retention story is this expiry. An enabled rule that
# expires nothing reads as a retention policy in the plan and delivers unbounded
# growth — and because this is now the one access-logs bucket for the account rather
# than one per environment, it takes every environment's traffic.
#
# `anytrue` is the safe direction here where `alltrue` is not: an absent expiration
# block collapses to `anytrue([])`, which is false, so deleting it goes red.
run "the_access_log_is_bounded_by_something" {
  command = plan

  assert {
    condition = anytrue([
      for r in aws_s3_bucket_lifecycle_configuration.access_logs.rule :
      r.status == "Enabled" && anytrue([for e in r.expiration : e.days > 0])
    ])
    error_message = "the access-logs bucket needs an enabled rule that actually expires objects — a rule with no expiration is a retention policy in name only, and this bucket is the account's, so it accumulates every environment's access records forever"
  }

  assert {
    condition     = aws_s3_bucket_lifecycle_configuration.access_logs.bucket == aws_s3_bucket.access_logs.id
    error_message = "the lifecycle configuration must apply to the access-logs bucket"
  }
}
