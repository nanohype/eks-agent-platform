# ADR 0006 — KEDA aws-sqs-queue uses pod-identity, not KEDA operator IRSA

## Status

Accepted (2026-05-16).

## Context

The KEDA SQS scaler needs AWS credentials with `sqs:GetQueueAttributes` on the per-tenant queue to read queue depth and decide scaling. KEDA supports three identity modes for AWS scalers:

1. **Operator IRSA** — KEDA's own operator pod has IRSA, single role granting `sqs:GetQueueAttributes` across all queues.
2. **Pod identity (`identityOwner: pod`)** — KEDA uses the workload's existing IRSA token. The workload's SA needs the SQS permission.
3. **Static credentials** — access key + secret in a Secret. Rejected on principle.

## Decision

Pod identity. The `ScaledObject` has `identityOwner: pod`; a paired `TriggerAuthentication` declares `podIdentity.provider: aws`. KEDA uses the workload's existing IRSA token (the `tenant-runtime` SA the operator provisions). The agent-iam baseline policy grants `sqs:GetQueueAttributes` + `sqs:ListQueueTags` on `*` with `aws:RequestedRegion` ABAC.

## Why pod-identity over operator-IRSA

1. **Single source of truth for tenant IAM.** Every tenant gets one IRSA role. `agent-iam` already maintains the baseline policy for Bedrock + CloudWatch Logs + (now) SQS read. Adding KEDA-SQS to the baseline keeps tenant IAM in one place.
2. **Per-tenant blast radius.** With operator-IRSA, KEDA's single role would need queue-read on every tenant's queue (`*` resource is the only viable scope; per-queue would mean updating KEDA's role on every tenant onboard). Compromising the KEDA operator pod means cluster-wide SQS read. With pod-identity, compromising a single tenant's pod means only that tenant's SQS access.
3. **Kill-switch consistency.** When the kill-switch fires, it detaches the baseline policy from the tenant role — this includes the SQS read. KEDA stops being able to read queue depth → scales to zero (or stays at zero). With operator-IRSA, the kill-switch would not affect KEDA's read capability; KEDA would keep scaling pods that immediately AccessDenied on Bedrock. Pod-identity makes the suspension story coherent.
4. **No extra IAM role to manage.** The KEDA chart values can stay default; we don't need to provision a KEDA operator IRSA role at all.

## Trade-offs

- **`sqs:GetQueueAttributes` granted with `Resource: "*"`.** The queue URL comes from `AgentFleet.spec.scaling.queueUrl` which the tenant controls. We can't scope the resource at the IAM policy level without per-fleet inline policies, which would complicate the kill-switch contract (the SFN detaches a single baseline policy; per-fleet inline policies would survive). The blast radius is bounded by:
  - `aws:RequestedRegion` ABAC matches the same allowed_regions Bedrock invoke is scoped to (so a tenant can't read queues in unrelated regions),
  - read-only permission (the tenant can't write to or delete other queues),
  - the kill-switch tag still suspends the entire baseline policy attachment.

  Acceptable. Documented in the inline comment in `terraform/components/agent-iam/main.tf`.

- **TriggerAuthentication needs to land before the ScaledObject.** KEDA tolerates this transiently (it retries reconciles), but we emit the `TriggerAuthentication` before the `ScaledObject` in `ensureKEDAScaledObject` to avoid a brief 'TA-not-found' status.

- **Each fleet creates its own TriggerAuthentication.** A cluster with 100 fleets has 100 TAs (one per fleet), all functionally identical. Could be a single namespace-scoped TA referenced by all fleets in that namespace, but the operator's `cleanupFleetResources` is simpler when the TA's lifecycle matches the fleet's.

## Alternatives considered

- **Operator IRSA.** Rejected for reasons 2 and 3.
- **Per-fleet inline IAM policy with the queue ARN scoped.** Rejected for kill-switch coherence.
- **Skip KEDA SQS and stay on CPU.** Workable but bad signal-to-noise: a fleet with high CPU utilization for unrelated reasons (e.g. a memory-intensive workload) would scale on the wrong signal. SQS depth is the actual demand signal.

## Cross-references

- Implementation: `operators/internal/controller/agentfleet_reconcile.go` (`ensureKEDAScaledObject`, `ensureKEDATriggerAuth`).
- Baseline policy: `terraform/components/agent-iam/main.tf` (`KEDASQSScalerRead` statement).
- Cross-component contract: ADR 0003 (tenant role naming).
