# @eks-agent/core

Shared runtime primitives:

- **Branded IDs** — `PlatformId`, `TenantId`, `AgentId`, etc. Compile-time enforcement that you don't pass a TenantId where a PlatformId is expected.
- **Zod schemas** — `PlatformSpec`, `ModelRouteSpec`, `CallEvent`, `TokenUsage`. Mirrors the operator CRDs for runtime validation when reading CRs back.
- **Error taxonomy** — `AgentError` + `ErrorClass` (`RateLimit | Overloaded | BadRequest | Server | Network | AuthFailure | GuardrailBlock | BudgetExceeded | ContextLengthExceeded | Cancelled`). `isRetryable(err)` is the canonical retry decision.

Zero AWS, zero Kubernetes — usable from any TS context.
