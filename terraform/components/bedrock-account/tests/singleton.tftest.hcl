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
run "the_one_configuration_points_at_the_buckets_this_component_owns" {
  command = plan

  assert {
    condition = alltrue([
      for c in aws_bedrock_model_invocation_logging_configuration.this.logging_config :
      alltrue([for s in c.s3_config : s.bucket_name == aws_s3_bucket.invocations.id])
    ])
    error_message = "the invocation logging configuration must deliver to this component's own bucket — there is one configuration per account, so pointing it anywhere else silently takes over or abandons the account's audit trail"
  }

  assert {
    condition = alltrue([
      for c in aws_bedrock_model_invocation_logging_configuration.this.logging_config :
      alltrue([for l in c.cloudwatch_config : l.log_group_name == aws_cloudwatch_log_group.invocations.name])
    ])
    error_message = "the CloudWatch half of the configuration must name this component's log group — the cost publisher subscribes to it through the SSM contract, and a mismatch leaves the subscription attached to a group nothing writes to"
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
