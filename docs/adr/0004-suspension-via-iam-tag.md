# ADR 0004 — Kill-switch suspension propagated via IAM tag, not EventBridge→k8s bridge

## Status

Accepted (2026-05-16).

## Context

When the `BudgetPolicy` reconciler detects a >120% breach, it publishes a `BudgetBreach` event to the kill-switch EventBridge bus. The bus's rule triggers a Step Functions state machine that needs to (a) revoke the tenant's Bedrock invoke permission and (b) cause the operator's k8s-side reconciler to mark the Platform as `Suspended` and scale fleets to zero.

(a) is straightforward — the SFN calls `iam:DetachRolePolicy`. (b) is the cross-system bridge problem: the SFN doesn't speak Kubernetes, the operator doesn't speak EventBridge.

Two architectural options for (b):

1. **Lambda subscribed to the bus calls the k8s API** — requires the Lambda to authenticate to the EKS cluster's API server (IRSA + aws-iam-authenticator mapping), patch the Platform CR with a suspension marker, and either the operator or a future controller acts on that marker.
2. **SFN modifies AWS state, operator detects drift** — the SFN tags the tenant IAM role with `agents.stxkxs.io/suspended=true`. The operator already calls `iam:GetRole` on every Platform reconcile (for IRSA verification); it reads the tag and propagates to `Platform.status.phase = Suspended` from there.

## Decision

Option 2. The SFN performs two IAM actions per breach: `DetachRolePolicy` (removes invoke capability) then `TagRole` (signals the operator). The operator's `PlatformReconciler.ensureIamRole` reads the role's tags via `iam:GetRole`, detects the marker, and:

- short-circuits before `attachBaselineIfMissing` (so the operator doesn't reattach the baseline on the next reconcile and undo the SFN's work),
- sets `Platform.status.phase = Suspended`, `status.suspendedAt`, `status.suspendedReason`,
- propagates to `AgentFleetReconciler` (which scales fleets to zero on the suspended phase).

Recovery is symmetric: ops removes the IAM tag manually (`aws iam untag-role`). The operator's next reconcile (≤60s with the production requeue interval) sees the cleared tag, reattaches the baseline, sets phase back to `Ready`, fleets rebuild.

## Why

1. **Loose coupling.** EventBridge → SFN → IAM is one architectural lane; IAM → k8s reconcile is another. Neither side needs to authenticate to the other.
2. **No cross-cluster auth complexity.** Multi-cluster deployments would otherwise need each Lambda to know each cluster's API endpoint + IAM mapping. The IAM-tag approach is identical across clusters because the operator on each cluster reads the same role.
3. **Recovery is a single CLI call.** `aws iam untag-role` from any operator's workstation. No `kubectl patch` against a Platform CR (which could be the wrong namespace, wrong cluster, or denied by RBAC during an incident).
4. **The detection latency is acceptable.** The kill-switch SLO is "fleet stops within minutes of breach", not seconds. The operator's 60s requeue interval (when IAM is wired) means worst-case detection is 60s + the next reconcile cycle, well inside the SLO.
5. **CloudTrail audit chain is uniform.** Both the SFN's tag-write and the operator's tag-read are CloudTrail events on the same IAM role. The audit trail for "who suspended this tenant" is one resource.

## Trade-offs

- **Detection lag of up to 60s.** If a tenant is invoking Bedrock at the moment the SFN runs, they get ~60s of stale in-flight requests succeeding before the operator detects the tag and tears down fleets. The IAM detach itself is immediate, so requests can fail with `AccessDenied` before that — but the pods remain running until the operator reconciles. Acceptable for a budget kill-switch; would not be acceptable for a security-critical kill-switch (which would warrant the Lambda-to-k8s-API path).
- **Operator must do `GetRole` on every Platform reconcile.** IAM rate limits per-account default to 100/sec which is well above our reconciler's rate, but a future high-scale deployment might need either IAM call coalescing or moving the suspension marker to a more efficient signal (DynamoDB conditional reads, e.g.).
- **The tag is reversible by anyone with `iam:UntagRole` on the path.** Recovery is intentionally easy, but it means the suspension isn't tamper-proof against an operator who panics and clears the tag. Mitigation: CloudTrail alerting on `UntagRole` events targeting the tenant-IAM-path so the un-suspension is at least noisy.

## Alternatives considered

- **Lambda → kubectl patch via aws-iam-authenticator.** Rejected for multi-cluster fragility (see Why #2).
- **Operator subscribes to SQS fed by EventBridge.** Workable but adds an SQS dependency, a goroutine in the operator, and a per-environment IAM grant. The IAM-tag path uses surface that already exists.
- **EventBridge rule targets a CloudWatch Logs entry the operator polls.** Polls add lag and don't simplify auth. Rejected.

## Cross-references

- Implementation: `terraform/components/kill-switch/main.tf` (`NormalizeInput` → `DetachBedrockPolicy` → `TagRoleSuspended` → `NotifyOperator`).
- Operator side: `operators/internal/controller/platform_iam.go` (`suspensionFromTags`, `ensureIamRole`).
- Cross-component contract (canonical): [ADR 0003 — Threat Model](./0003-threat-model.md#cross-component-contract-kill-switch-suspension-marker).
- Runbook: [`docs/runbooks/platform-suspended.md`](../runbooks/platform-suspended.md).
- Flow diagram: [`docs/architecture/kill-switch-flow.md`](../architecture/kill-switch-flow.md).
