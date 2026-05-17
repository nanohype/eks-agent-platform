# Architecture Decision Records

| ADR                                      | Decision                                                                                                         | Status   |
| ---------------------------------------- | ---------------------------------------------------------------------------------------------------------------- | -------- |
| [0001](./0001-monorepo.md)               | Monorepo over multi-repo                                                                                         | Accepted |
| [0002](./0002-bedrock-only-v1.md)        | Bedrock-only model plane in v1                                                                                   | Accepted |
| [0003](./0003-threat-model.md)           | Operator IAM blast radius, STRIDE, cross-component contracts (tenant role naming, kill-switch suspension marker) | Accepted |
| [0004](./0004-suspension-via-iam-tag.md) | Kill-switch suspension propagated via IAM tag, not EventBridge→k8s bridge                                        | Accepted |
| [0005](./0005-cost-publisher-lambda.md)  | In-flight Bedrock cost via Lambda republisher, not direct CloudWatch metric filter                               | Accepted |
| [0006](./0006-keda-pod-identity.md)      | KEDA aws-sqs-queue uses pod-identity, not KEDA operator IRSA                                                     | Accepted |
| [0007](./0007-eval-runtime-kustomize.md) | eval-runtime WorkflowTemplate ships via kustomize, not the operator chart                                        | Accepted |

## Template

Each ADR follows the shape:

```
# ADR <number> — <decision title>

## Status
Accepted | Superseded by ADR-N | Rejected (YYYY-MM-DD).

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

New ADRs land at the next sequential number. Don't reuse numbers when ADRs get superseded — mark the old one with "Superseded by ADR-N" and write the new ADR fresh.
