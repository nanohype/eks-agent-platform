data "aws_iam_role" "karpenter_node" {
  name = var.node_role_name
}

locals {
  prefix = "${var.environment}-${var.cluster_name}-accel"
  tags = merge(var.tags, {
    Component = "accelerator-pools"
    Tier      = "platform"
  })

  oidc_host = replace(var.oidc_issuer, "https://", "")
}

################################################################################
# Karpenter node role extensions
#
# Karpenter manages the cluster's node IAM role centrally (in landing-zone).
# We attach additional inline policies needed by accelerator workloads:
# - Neuron device plugin needs DescribeInstances for topology discovery
# - NVIDIA GPU Operator needs container-toolkit installation perms (already
#   covered by the AWS EKS-managed node role baseline)
################################################################################

resource "aws_iam_role_policy" "neuron_topology" {
  name = "neuron-topology"
  role = data.aws_iam_role.karpenter_node.name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeRegions"
      ]
      Resource = "*"
    }]
  })
}

################################################################################
# IRSA roles for the device plugins / operators
#
# Both the NVIDIA GPU Operator and the AWS Neuron device plugin run as
# in-cluster controllers that need a small set of AWS permissions for
# instance introspection.
################################################################################

resource "aws_iam_role" "neuron" {
  name = "${local.prefix}-neuron"
  path = "/eks-agent-platform/"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = var.oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_host}:sub" = "system:serviceaccount:${var.neuron_addon_namespace}:neuron-device-plugin"
          "${local.oidc_host}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "neuron" {
  name = "neuron-introspection"
  role = aws_iam_role.neuron.id

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

resource "aws_iam_role" "gpu_operator" {
  name = "${local.prefix}-gpu-operator"
  path = "/eks-agent-platform/"
  tags = local.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = var.oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_host}:sub" = "system:serviceaccount:${var.gpu_operator_namespace}:gpu-operator"
          "${local.oidc_host}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })
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

################################################################################
# SSM outputs — read by the operator + by gitops Helm values
################################################################################

resource "aws_ssm_parameter" "neuron_role_arn" {
  name  = "/eks-agent-platform/${var.environment}/accelerator-pools/neuron_role_arn"
  type  = "String"
  value = aws_iam_role.neuron.arn
  tags  = local.tags
}

resource "aws_ssm_parameter" "gpu_operator_role_arn" {
  name  = "/eks-agent-platform/${var.environment}/accelerator-pools/gpu_operator_role_arn"
  type  = "String"
  value = aws_iam_role.gpu_operator.arn
  tags  = local.tags
}

################################################################################
# Static catalog of accelerator pool defaults — consumed by AcceleratorPool
# CRs in the operator. Stored as JSON in SSM so the controller can fetch the
# canonical instance-type + capacity-type list without hard-coding.
################################################################################

resource "aws_ssm_parameter" "pool_catalog" {
  name = "/eks-agent-platform/${var.environment}/accelerator-pools/catalog"
  type = "String"
  tags = local.tags

  value = jsonencode({
    pools = [
      {
        name           = "nvidia-l4"
        family         = "nvidia"
        instance_types = ["g6.xlarge", "g6.2xlarge", "g6.4xlarge", "g6.12xlarge"]
        capacity_types = ["spot", "on-demand"]
        device_class   = "gpu.nvidia.com"
        labels = {
          "nvidia.com/gpu.product" = "NVIDIA-L4"
        }
      },
      {
        name           = "nvidia-l40s"
        family         = "nvidia"
        instance_types = ["g6e.xlarge", "g6e.2xlarge", "g6e.4xlarge", "g6e.12xlarge"]
        capacity_types = ["spot", "on-demand"]
        device_class   = "gpu.nvidia.com"
        labels = {
          "nvidia.com/gpu.product" = "NVIDIA-L40S"
        }
      },
      {
        name           = "nvidia-h100"
        family         = "nvidia"
        instance_types = ["p5.48xlarge"]
        capacity_types = ["on-demand"]
        device_class   = "gpu.nvidia.com"
        labels = {
          "nvidia.com/gpu.product" = "NVIDIA-H100-80GB-HBM3"
        }
      },
      {
        name           = "neuron-inf2"
        family         = "neuron"
        instance_types = ["inf2.xlarge", "inf2.8xlarge", "inf2.24xlarge", "inf2.48xlarge"]
        capacity_types = ["on-demand"]
        device_class   = "neuron.aws.com"
        labels = {
          "aws.amazon.com/neuron" = "true"
        }
      },
      {
        name           = "neuron-trn2"
        family         = "neuron"
        instance_types = ["trn2.48xlarge"]
        capacity_types = ["on-demand"]
        device_class   = "neuron.aws.com"
        labels = {
          "aws.amazon.com/neuron" = "true"
        }
      }
    ]
  })
}
