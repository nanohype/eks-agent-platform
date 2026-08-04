# Intentionally empty.
#
# This component exports nothing, because nothing consumes it. It grants the GPU
# Operator an AWS identity, and the binding is made here by
# aws_eks_pod_identity_association — so there is no role ARN for a consumer to
# paste anywhere, which is the seam EKS Pod Identity exists to remove.
#
# It previously exported two role ARNs and an SSM path to a static catalog of
# accelerator pool defaults. Nothing read any of the three.
#
# The file stays because tflint's terraform_standard_module_structure rule
# expects an outputs.tf in every module, empty or otherwise.
