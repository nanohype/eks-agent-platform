# Architecture Decision Records

| ADR                                           | Decision                                                                                                         | Status                 |
| --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- | ---------------------- |
| [0001](./0001-monorepo.md)                    | Monorepo over multi-repo                                                                                         | Accepted               |
| [0002](./0002-bedrock-only-v1.md)             | Bedrock-only model plane in v1                                                                                   | Accepted               |
| [0003](./0003-threat-model.md)                | Operator IAM blast radius, STRIDE, cross-component contracts (tenant role naming, kill-switch suspension marker) | Accepted               |
| [0004](./0004-suspension-via-iam-tag.md)      | Kill-switch suspension propagated via IAM tag, not EventBridge→k8s bridge                                        | Accepted               |
| [0005](./0005-cost-publisher-lambda.md)       | In-flight Bedrock cost via Lambda republisher, not direct CloudWatch metric filter                               | Accepted               |
| [0006](./0006-keda-pod-identity.md)           | KEDA aws-sqs-queue uses pod-identity, not KEDA operator IRSA                                                     | Accepted               |
| [0007](./0007-eval-runtime-operator-chart.md) | The eval-runtime ships in the operator chart, not a gitops overlay                                               | Accepted               |
| [0008](./0008-vcluster-isolation-tier.md)     | vcluster hard-isolation tier: reconcile model, synced-SA Pod Identity, ArgoCD destination, containment, teardown | Accepted               |
| [0009](./0009-slo-hold-single-writer.md)      | Burn-rate rollout hold: one evaluator for the objective, one writer for the AppProject                           | Accepted               |

## Template

Each ADR follows the shape:

```
# ADR <number> — <decision title>

## Status
Accepted | Rejected (YYYY-MM-DD).

## Context
The problem + the forces in play.

## Decision
What we chose. One paragraph.

## Why
Numbered rationale. Be specific.

## Trade-offs
What we gave up. Be honest.

## Alternatives considered
What we rejected and why.

## Cross-references
Implementation pointers + related docs + other ADRs.
```

New ADRs land at the next sequential number. Every ADR here describes a decision the
repository currently implements, so there is no superseded state to record: an ADR whose
mechanism the repo no longer uses is rewritten to describe the one it does, or removed and
the remaining numbers closed up so the sequence has no gaps.
