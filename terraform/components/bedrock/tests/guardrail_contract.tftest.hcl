# What this component publishes, and where.
#
# The guardrail is the only thing here that is genuinely per-cluster: a guardrail is a
# named resource, so an account holds many and each cluster gets its own. Everything
# about invocation logging is an account+region singleton owned by bedrock-account, and
# is read from the account contract by the one component that needs it.
#
# The keys are asserted by their FULL name, not their prefix. The last segment is what
# the operator matches on — it decodes `bedrock/baseline_guardrail_id` by that exact
# string — so `baseline_guardrailid` publishes successfully, sweeps successfully, and
# decodes to an empty field. A prefix assertion cannot see that.
#
# The literals are repeated here rather than derived from the resource. This is a
# contract across two languages, and Go pins the same strings independently; a test that
# read the name off the resource would agree with whatever spelling the resource
# happened to use.
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

run "the_guardrail_handles_land_on_the_keys_the_operator_decodes" {
  command = plan

  assert {
    condition     = one(aws_ssm_parameter.baseline_guardrail_id).name == "/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_id"
    error_message = "the guardrail id key must be exactly bedrock/baseline_guardrail_id — the operator matches that string, and a near-miss publishes and sweeps cleanly while decoding to nothing, leaving every ModelGateway route without a default guardrail and no error anywhere"
  }

  # The version is not decoration. An invocation pins a guardrail VERSION, so a consumer
  # holding the id alone cannot name what it is applying.
  assert {
    condition     = one(aws_ssm_parameter.baseline_guardrail_version).name == "/eks-agent-platform/${var.cluster_name}/bedrock/baseline_guardrail_version"
    error_message = "the guardrail version key must be exactly bedrock/baseline_guardrail_version — an invocation pins a version, so publishing the id alone leaves the consumer unable to name what it applies"
  }

  # Under this cluster's subtree and no other. The operator's whole configuration is one
  # recursive GetParametersByPath of /eks-agent-platform/<cluster>/, so a key published
  # anywhere else is invisible to it — and invisible is indistinguishable from absent,
  # which the operator reads as a field nobody set rather than as an error.
  assert {
    condition = alltrue([
      for p in concat(aws_ssm_parameter.baseline_guardrail_id, aws_ssm_parameter.baseline_guardrail_version) :
      startswith(p.name, "/eks-agent-platform/${var.cluster_name}/bedrock/")
    ])
    error_message = "every published key must sit under this cluster's bedrock prefix — the operator sweeps that subtree and nothing else"
  }

  # And each value comes off the guardrail this component actually created. A version
  # published from any other source can name one that does not exist, and that surfaces
  # on the invocation rather than on the apply — the route resolves, the call is made,
  # and Bedrock rejects a version nobody can see in terraform.
  assert {
    condition = alltrue([
      one(aws_ssm_parameter.baseline_guardrail_id).value == one(aws_bedrock_guardrail.baseline).guardrail_id,
      one(aws_ssm_parameter.baseline_guardrail_version).value == one(aws_bedrock_guardrail.baseline).version,
    ])
    error_message = "each published handle must carry the created guardrail's own id and version — a value from any other source names something that may not exist, and it fails on the invocation rather than on the apply"
  }
}

# Guardrails are not available in every region, so the baseline is toggleable. What must
# not happen is a partial publish: a key present with an empty value reads to the
# operator as a configured guardrail whose id happens to be blank, and it would apply
# that to a route rather than fall through to the route's own reference.
run "a_region_without_guardrails_publishes_nothing_rather_than_blanks" {
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
      length(aws_ssm_parameter.baseline_guardrail_id) == 0,
      length(aws_ssm_parameter.baseline_guardrail_version) == 0,
    ])
    error_message = "with the baseline off, neither guardrail key may be published — a key carrying an empty string reads to the operator as a configured guardrail with a blank id, and it would apply that in place of the route's own reference"
  }
}
