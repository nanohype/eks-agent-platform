variable "environment" {
  description = "Environment name. Selects the unconditional teardown posture in development."
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name — used to namespace SSM parameters and tags"
  type        = string

  validation {
    condition     = length(var.cluster_name) <= 27
    error_message = "cluster_name (<environment>-<base>) must be <= 27 chars: it prefixes cluster-scoped S3 bucket names (e.g. <cluster_name>-bedrock-invocations-<account>) that must stay within S3's 63-char limit."
  }
}

variable "logs_kms_key_arn" {
  description = "KMS key ARN for encrypting Bedrock invocation logs (cmk-logs from landing-zone)"
  type        = string
}

variable "log_retention_days" {
  description = "How long to retain Bedrock invocation logs in CloudWatch"
  type        = number
  default     = 365
}

variable "object_lock_mode" {
  description = <<-EOT
    Object Lock mode on the invocation logging bucket. This is a retention decision about the
    model invocation record, deliberately outside force_destroy_buckets — a teardown flag must
    not be able to talk over a compliance statement.

    GOVERNANCE (default): the retention holds, and a principal carrying
    `s3:BypassGovernanceRetention` can clear it so the environment tears down. Nothing in this
    repo or in landing-zone grants that action, so the destroy depends on the caller already
    having it. An operator running a teardown without it sees the bucket refuse, object by
    object, with the lock as the reason.

    COMPLIANCE: nothing and nobody can shorten the retention, including the root account. The
    bucket, and therefore this component, cannot be destroyed until every object's
    retain-until date has passed. Choose it when that is the point, not to be thorough.
  EOT
  type        = string
  default     = "GOVERNANCE"
  validation {
    condition     = contains(["GOVERNANCE", "COMPLIANCE"], var.object_lock_mode)
    error_message = "object_lock_mode must be GOVERNANCE or COMPLIANCE."
  }
}

variable "object_lock_retention_days" {
  description = "How long to lock objects in the invocation logging bucket"
  type        = number
  default     = 365
}

variable "access_logs_retention_days" {
  description = "Retention for S3 server-access logs in the access-logs bucket"
  type        = number
  default     = 365
}

variable "enable_guardrails_baseline" {
  description = "Create a baseline Bedrock Guardrail that the operator can reference. Tenant-specific guardrails attach per route via ModelGateway guardrailRef at reconcile time."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Common tags applied to all resources"
  type        = map(string)
  default     = {}
}

variable "force_destroy_buckets" {
  description = <<-EOT
    Allow this component's access-logs bucket to be destroyed while it still holds objects, in
    any environment. Development already allows it unconditionally; this is the opt-in for
    everywhere else.

    It exists because a cluster here is an agent-managed, often short-lived thing — eks-fleet
    vends spokes with a ttlDays and a hub reaper that deletes them on expiry — so a teardown is
    an ordinary lifecycle event rather than an emergency. S3 server-access logs land from the
    first PUT the invocations bucket takes, so without this a reverse teardown wedges on
    BucketNotEmpty almost immediately and leaves the cluster, VPC and NAT gateways standing and
    billing, which is the outcome the teardown existed to prevent.

    Deliberately two acts, not one flag: force_destroy has no effect until a successful apply
    lands it in state, so an operator (or an agent) must apply with this set and only then
    destroy. There is no single command that reaches a populated production bucket.

    What it exposes: server-access logs for the invocations bucket — who read the model
    invocation record, and when. The invocations bucket itself is governed by object_lock_mode,
    not by this, because a WORM retention is a compliance statement and a teardown flag must not
    be able to talk over it.
  EOT
  type        = bool
  default     = false
}
