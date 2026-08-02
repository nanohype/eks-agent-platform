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

variables {
  environment      = "staging"
  region           = "us-west-2"
  cluster_name     = "staging-platform"
  data_kms_key_arn = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000"
  tags             = {}
}

# Athena reaches S3 and KMS under the CALLER's identity, and the caller is the
# operator role this policy attaches to. Reading the SSE-KMS estimates and writing
# the SSE-KMS result set are both on this key, and the workgroup enforces encrypted
# results — so Decrypt alone leaves every query failing at the write step, the
# reconciler returns before the kill-switch block, and a tenant at 120% of budget
# never trips it with nothing going red.
run "the_operator_can_decrypt_and_write_what_it_queries" {
  command = plan

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.operator_cost.policy).Statement :
      contains(s.Action, "kms:Decrypt")
      && contains(s.Action, "kms:GenerateDataKey")
      && contains(tolist(s.Resource), var.data_kms_key_arn)
    ])
    error_message = "the operator role must hold kms:Decrypt AND kms:GenerateDataKey on the cost data key — without both, StartQueryExecution returns FAILED on every tick and a tenant over budget never trips the kill switch"
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
        aws_ssm_parameter.cur_bucket,
        aws_ssm_parameter.athena_workgroup,
        aws_ssm_parameter.athena_database,
        aws_ssm_parameter.athena_results_bucket,
        aws_ssm_parameter.cur_table_name,
        aws_ssm_parameter.estimate_table_name,
        aws_ssm_parameter.reconciliation_view,
      ] : startswith(p.name, "/eks-agent-platform/${var.cluster_name}/cost-pipeline/")
    ])
    error_message = "every republished handle must sit under this cluster's prefix — the operator's entire configuration is one GetParametersByPath sweep of that subtree, so a parameter published anywhere else is invisible to it"
  }

  # athena_results_bucket is in that list deliberately. The operator has always
  # decoded this key and nothing ever published it, so the field was permanently
  # empty — a seam that claimed more than it delivered.
  assert {
    condition     = aws_ssm_parameter.athena_results_bucket.name == "/eks-agent-platform/${var.cluster_name}/cost-pipeline/athena_results_bucket"
    error_message = "athena_results_bucket must be published — the operator reads it and previously received nothing"
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
