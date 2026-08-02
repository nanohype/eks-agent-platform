# Teardown posture for bedrock.
#
# Two buckets, two different contracts, and the point of this suite is that they stay
# different. access-logs moves on force_destroy_buckets like every other operational bucket in
# the org. The invocations bucket does not: its force_destroy tracks object_lock_mode, because
# a WORM retention is a compliance statement about the model invocation record and a teardown
# flag must not be able to talk over it.
#
# Folding the two would be the easy mistake — one lever, every gate is the rule everywhere
# else. Here it would make a COMPLIANCE bucket destroyable-in-intent, which is the one thing
# COMPLIANCE means it is not. The assertions below pin both halves so neither drifts into the
# other.
#
# Root-module resources, so an assert reads the attribute the provider will send.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/test"
      user_id    = "AIDTEST"
    }
  }
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock-role"
    }
  }
}

variables {
  environment      = "staging"
  cluster_name     = "staging-platform"
  logs_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000"
  tags             = {}
}

run "protected_environment_keeps_the_access_logs_bucket" {
  command = plan

  assert {
    condition     = aws_s3_bucket.access_logs.force_destroy == false
    error_message = "access-logs must not be force-destroyable without the lever"
  }
}

run "force_destroy_buckets_opens_the_access_logs_bucket" {
  command = plan

  variables {
    force_destroy_buckets = true
  }

  assert {
    condition     = aws_s3_bucket.access_logs.force_destroy
    error_message = "force_destroy_buckets must open access-logs — server-access logs land from the first PUT, so it is non-empty long before a teardown is attempted"
  }
}

run "development_is_unconditionally_tearable_down" {
  command = plan

  variables {
    environment  = "development"
    cluster_name = "development-platform"
  }

  assert {
    condition     = aws_s3_bucket.access_logs.force_destroy
    error_message = "development must tear down without an opt-in"
  }
}

# The carve-out, asserted in both directions. GOVERNANCE is clearable by a principal holding
# s3:BypassGovernanceRetention, so the bucket stays destroyable; COMPLIANCE is not clearable by
# anyone including the root account, so it must not claim to be.
run "invocations_tracks_object_lock_not_the_teardown_lever" {
  command = plan

  variables {
    object_lock_mode      = "COMPLIANCE"
    force_destroy_buckets = true
  }

  assert {
    condition     = aws_s3_bucket.invocations.force_destroy == false
    error_message = "a COMPLIANCE-locked invocations bucket must stay force_destroy=false even with the teardown lever set — nothing can shorten that retention, so claiming otherwise puts a destroy in the plan that AWS will refuse object by object"
  }

  assert {
    condition     = aws_s3_bucket.access_logs.force_destroy
    error_message = "the object-lock mode governs the invocations bucket only — access-logs still moves on the lever"
  }
}

run "governance_mode_leaves_invocations_destroyable" {
  command = plan

  variables {
    object_lock_mode = "GOVERNANCE"
  }

  assert {
    condition     = aws_s3_bucket.invocations.force_destroy
    error_message = "under GOVERNANCE the retention is clearable by a principal carrying s3:BypassGovernanceRetention, so the bucket stays destroyable"
  }
}
