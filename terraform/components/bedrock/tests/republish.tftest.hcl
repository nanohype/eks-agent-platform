# The republish, asserted against DISTINCT values on purpose.
#
# On the invocation-logging side this component's entire job is to copy two
# account-scoped values into the subtree the operator sweeps. Copying is the one
# operation a shared fixture cannot check: if every source parameter resolves to the
# same string — which is what a `mock_data "aws_ssm_parameter"` default does — then a
# suite passes exactly as happily when the two are transposed, and the kill switch ends
# up subscribing to a bucket ARN.
#
# So there is deliberately NO shared default here. Each source is overridden with its
# own sentinel, and each assertion names which sentinel it expects.
#
# This component creates no S3 bucket, which is worth stating because it is why this
# file exists: the suite runner used to be reached only through the teardown gate's
# bucket predicate, so a component like this one could carry any number of tests and
# never have them executed.

mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-west-2"
    }
  }
}

variables {
  cluster_name = "development-platform"
  tags         = {}
}

run "each_account_value_lands_on_its_own_cluster_key" {
  command = plan

  override_data {
    target = data.aws_ssm_parameter.account_invocation_log_group
    values = { value = "SENTINEL-account-log-group" }
  }

  override_data {
    target = data.aws_ssm_parameter.account_invocation_bucket_arn
    values = { value = "SENTINEL-account-bucket-arn" }
  }

  assert {
    condition     = nonsensitive(aws_ssm_parameter.invocation_log_group.value) == "SENTINEL-account-log-group"
    error_message = "the republished log-group key must carry the account's log-group value — kill-switch subscribes a metric filter to whatever this holds, and a transposed copy attaches it to a bucket ARN, which fails as a name rather than as a wiring error"
  }

  assert {
    condition     = nonsensitive(aws_ssm_parameter.invocation_bucket.value) == "SENTINEL-account-bucket-arn"
    error_message = "the republished bucket key must carry the account's invocation-bucket ARN"
  }
}

# Where the values go. The operator's whole configuration is one recursive
# GetParametersByPath of /eks-agent-platform/<cluster>/, so a parameter published
# anywhere else is invisible to it — and invisible is indistinguishable from absent,
# which the operator reads as an unconfigured field rather than an error.
run "the_republished_keys_sit_in_the_subtree_the_operator_sweeps" {
  command = plan

  assert {
    condition = alltrue([
      for p in [
        aws_ssm_parameter.invocation_log_group,
        aws_ssm_parameter.invocation_bucket,
      ] : startswith(p.name, "/eks-agent-platform/${var.cluster_name}/bedrock/")
    ])
    error_message = "every republished key must sit under this cluster's bedrock prefix — the operator sweeps that subtree and nothing else, so a key published elsewhere reads to it as a field nobody set"
  }

  # The full key, not just the prefix. The prefix assertion above cannot see a typo in
  # the last segment, and the last segment is the part the operator matches on: it
  # decodes `bedrock/invocation_log_group` by that exact string, so `invocation_loggroup`
  # publishes successfully, sweeps successfully, and decodes to an empty field.
  #
  # The literal is repeated here on purpose. It is a contract across two languages —
  # Go pins the same string independently — and a test that derived it from the
  # resource would agree with any spelling the resource happened to use.
  assert {
    condition     = aws_ssm_parameter.invocation_log_group.name == "/eks-agent-platform/${var.cluster_name}/bedrock/invocation_log_group"
    error_message = "the log-group key must be exactly bedrock/invocation_log_group — the operator matches that string, and a near-miss publishes and sweeps cleanly while decoding to nothing"
  }

  assert {
    condition     = aws_ssm_parameter.invocation_bucket.name == "/eks-agent-platform/${var.cluster_name}/bedrock/invocation_bucket_arn"
    error_message = "the bucket key must be exactly bedrock/invocation_bucket_arn"
  }
}

# Where the values come from. This is the other half of the same claim: the point of
# the republish is that the source is the ONE component that owns the singleton, so a
# source key drifting to a cluster subtree would quietly restore the per-cluster
# ownership this split exists to remove.
run "the_source_values_come_from_the_account_subtree" {
  command = plan

  assert {
    condition = alltrue([
      for n in [
        data.aws_ssm_parameter.account_invocation_log_group.name,
        data.aws_ssm_parameter.account_invocation_bucket_arn.name,
      ] : startswith(n, "/eks-agent-platform/org/bedrock-account/")
    ])
    error_message = "the republish must read from the account contract — reading a cluster subtree instead would make each cluster republish its own copy and restore the per-cluster ownership of an account+region singleton"
  }
}

# The guardrail is genuinely per-cluster and region-gated; the republish is neither.
# Tying them together would mean a cluster in a region without Guardrails support
# silently published no invocation handles either, and the operator would read the
# whole bedrock path as unset because of an unrelated feature's availability.
run "the_republish_survives_a_region_without_guardrails" {
  command = plan

  variables {
    enable_guardrails_baseline = false
  }

  assert {
    condition     = length(aws_bedrock_guardrail.baseline) == 0
    error_message = "the guardrail must be absent when the toggle is off"
  }

  assert {
    condition = alltrue([
      for p in [
        aws_ssm_parameter.invocation_log_group,
        aws_ssm_parameter.invocation_bucket,
      ] : startswith(p.name, "/eks-agent-platform/${var.cluster_name}/bedrock/")
    ])
    error_message = "the invocation-logging republish must not be gated on the guardrail — they are unrelated, and coupling them makes an unsupported region look like an unconfigured operator"
  }
}
