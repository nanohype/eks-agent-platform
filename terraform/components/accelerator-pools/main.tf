locals {
  prefix = "${var.cluster_name}-accel"
  tags = merge(var.tags, {
    Component = "accelerator-pools"
    Tier      = "platform"
  })
}

################################################################################
# The GPU Operator's AWS identity.
#
# This component is the AWS half of accelerator scheduling and nothing more. The
# Karpenter NodePool and the DRA DeviceClass are Kubernetes resources and live in
# the eks-gitops accelerators category; the nodes themselves are Karpenter's, and
# it launches one only when a pod asks for nvidia.com/gpu. So applying this costs
# nothing — it grants an identity, it does not reserve capacity.
#
# There is no Neuron counterpart. eks-gitops installs the GPU Operator and the
# NVIDIA DRA driver; it has no Neuron device plugin addon, so a role bound to a
# neuron-device-plugin ServiceAccount would have been bound to a ServiceAccount
# nothing creates — an identity for a workload that is not deployed, which reads
# to anyone auditing IAM as though Neuron were supported. Inferentia and Trainium
# come back as an eks-gitops addon first, and this component follows it.
################################################################################

resource "aws_iam_role" "gpu_operator" {
  name = "${local.prefix}-gpu-operator"
  path = "/eks-agent-platform/"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
    }]
  })
}

# EKS Pod Identity binds this role to the gpu-operator ServiceAccount. The name
# and namespace are the NVIDIA chart's own defaults, and the namespace is what
# the eks-gitops addons-accelerators-helm ApplicationSet installs into. A
# mismatch here is silent: the association applies cleanly against a
# ServiceAccount that does not exist, and the operator falls back to no
# credentials rather than failing.
resource "aws_eks_pod_identity_association" "gpu_operator" {
  cluster_name    = var.cluster_name
  namespace       = var.gpu_operator_namespace
  service_account = "gpu-operator"
  role_arn        = aws_iam_role.gpu_operator.arn
  tags            = local.tags
}

resource "aws_iam_role_policy" "gpu_operator" {
  name = "gpu-operator-introspection"
  role = aws_iam_role.gpu_operator.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceTypes"
      ]
      Resource = "*"
    }]
  })
}
