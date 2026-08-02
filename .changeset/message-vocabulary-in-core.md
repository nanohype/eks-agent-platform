---
'@eks-agent/core': minor
---

`@eks-agent/core` now carries the model-call message vocabulary: `Message` (with the prompt-cache breakpoint flag) and the `StopReason` union, exported from a new `messages` module.

These are plain types, not zod schemas, and the CRD drift gate deliberately does not see them — they describe a request a caller builds and a field it reads back, not a resource parsed off the API server.

`guardrail_intervened` stays in the same union as `end_turn` rather than being signalled separately, so a caller has to account for a blocked exchange in the same place it accounts for a normal completion.
