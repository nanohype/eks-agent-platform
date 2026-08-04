# What this component is, asserted.
#
# It grants one identity and reserves nothing. Every failure it can have is a
# silent one — an association that applies cleanly against a ServiceAccount
# nothing creates, a trust policy that names the wrong principal, a role that
# quietly grows past introspection — so each is stated here rather than left to
# be noticed on a cluster that does not exist yet.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/test"
      user_id    = "AIDTEST"
    }
  }
  mock_data "aws_region" {
    defaults = { region = "us-west-2" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/mock-role" }
  }
}

variables {
  cluster_name = "development-agents"
  tags         = {}
}

# The binding is the whole component, and it is the half that fails silently.
#
# EKS accepts an association naming a ServiceAccount that does not exist — it is
# a forward reference by design, since the association is usually applied before
# the workload. So a wrong name or a wrong namespace applies green, and the GPU
# Operator runs with no credentials and no error anywhere. The counterpart lives
# in eks-gitops: applicationsets/addons-accelerators-helm.yaml installs the
# NVIDIA chart with appName/namespace gpu-operator, and the chart's own operator
# ServiceAccount is gpu-operator.
run "the_association_names_the_service_account_the_chart_creates" {
  command = plan

  assert {
    condition     = aws_eks_pod_identity_association.gpu_operator.service_account == "gpu-operator"
    error_message = "the association must name the gpu-operator ServiceAccount — EKS accepts an association against a ServiceAccount that does not exist, so a wrong name applies green and leaves the GPU Operator running with no credentials and nothing anywhere reporting it"
  }

  assert {
    condition     = aws_eks_pod_identity_association.gpu_operator.namespace == "gpu-operator"
    error_message = "the association must sit in the namespace the eks-gitops addons-accelerators-helm ApplicationSet installs the chart into — a ServiceAccount of the right name in the wrong namespace is a different ServiceAccount, and binds nothing"
  }

  assert {
    condition     = aws_eks_pod_identity_association.gpu_operator.cluster_name == var.cluster_name
    error_message = "the association must be made against this cluster — an association on another cluster's name is accepted and binds nothing here"
  }

  # And it binds the role this component owns, not some other ARN.
  assert {
    condition     = aws_eks_pod_identity_association.gpu_operator.role_arn == aws_iam_role.gpu_operator.arn
    error_message = "the association must bind the role this component creates — pointing it elsewhere leaves this role unused and grants the GPU Operator whatever the other role holds"
  }
}

# Pod Identity, not IRSA. The trust policy decides which of the two is in play,
# and the wrong principal is the difference between a working binding and a role
# nothing can assume.
run "the_role_trusts_pod_identity_and_not_a_web_identity" {
  command = plan

  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role.gpu_operator.assume_role_policy).Statement :
      try(s.Principal.Service, null) == "pods.eks.amazonaws.com"
    ])
    error_message = "the role must trust pods.eks.amazonaws.com and nothing else — any other principal means the Pod Identity agent cannot assume it, and an OIDC federated principal here would be the IRSA seam this component does not use"
  }

  # TagSession as well as AssumeRole. Pod Identity passes session tags on every
  # assumption, and a trust policy allowing only AssumeRole fails at the call.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role.gpu_operator.assume_role_policy).Statement :
      contains(s.Action, "sts:AssumeRole") && contains(s.Action, "sts:TagSession")
    ])
    error_message = "the trust policy must allow both sts:AssumeRole and sts:TagSession — Pod Identity tags every session, so AssumeRole alone is refused at the call rather than at apply"
  }
}

# Introspection only. This role exists to let the GPU Operator read what kind of
# instance it is on; anything past that is a standing grant on a workload that
# runs on every accelerator node.
run "the_grant_reaches_no_further_than_introspection" {
  command = plan

  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.gpu_operator.policy).Statement :
      alltrue([for a in s.Action : startswith(a, "ec2:Describe")])
    ]) && length(jsondecode(aws_iam_role_policy.gpu_operator.policy).Statement) > 0
    error_message = "every action in this policy must be an ec2:Describe* — the GPU Operator reads instance metadata and nothing else, and a wildcard or a mutating action here is a standing grant on a DaemonSet that runs on every accelerator node"
  }

  # The length guard above is not decoration: alltrue([]) is TRUE, so a policy
  # with no statements at all would satisfy the complement while granting
  # nothing and leaving the operator unable to introspect.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.gpu_operator.policy).Statement :
      contains(s.Action, "ec2:DescribeInstances") && contains(s.Action, "ec2:DescribeInstanceTypes")
    ])
    error_message = "the policy must actually grant DescribeInstances and DescribeInstanceTypes — the narrowing assertion above is satisfied by an empty policy, which would be a role that assumes cleanly and can read nothing"
  }
}

# What this component no longer does is NOT asserted here, and cannot be.
#
# It published two role ARNs and a JSON pool catalog to SSM and nothing read any
# of them. The ARNs are the IRSA paste seam Pod Identity removes — the binding is
# made above, so no consumer needs the ARN — and the catalog described an
# AcceleratorPool CR that was never built.
#
# A tofu test cannot state the absence of a resource: an assertion has to name
# one, and naming a resource that is not in the configuration is a static error
# rather than a failing run. "This component publishes no orphan parameter" is a
# property of the whole tree anyway — one parameter written here and read two
# components away is fine, one written and read nowhere is not — so it belongs in
# the SSM contract sweep across every component, not in this file.
