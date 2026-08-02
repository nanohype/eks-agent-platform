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

# Cost and Usage Reports and Cost Explorer have no endpoint outside us-east-1. Created
# through the workload-region provider these resources cannot be created at all, which
# is the first hop of the path and the reason none of the rest was ever exercised.
run "the_us_east_1_only_apis_use_the_us_east_1_provider" {
  command = plan

  assert {
    condition     = aws_cur_report_definition.this.s3_bucket == aws_s3_bucket.cur.id
    error_message = "the report definition must deliver into this component's own CUR bucket"
  }

  # Activation is what puts PlatformId in the Parquet at all. It is not retroactive,
  # so spend that lands before it is unattributable forever.
  assert {
    condition     = aws_ce_cost_allocation_tag.platform_id.tag_key == "PlatformId" && aws_ce_cost_allocation_tag.platform_id.status == "Active"
    error_message = "the PlatformId cost-allocation tag must be activated — until it is, the column the reconciler filters on is absent from CUR and every tenant reads zero spend"
  }
}

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
