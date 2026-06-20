---
'@eks-agent/sdk': minor
'@eks-agent/pricing': minor
'@eks-agent/core': minor
---

Harden the Bedrock call path and make unpriced traffic observable.

- `@eks-agent/sdk`: every `BedrockAdapter.messages()` call now carries a bounded request deadline even when the caller passes no `AbortSignal`. New `requestTimeoutMs` option (default 60s); a caller-supplied signal is combined with the deadline rather than replacing it, and a deadline fire classifies as a retryable `Network` error.
- `@eks-agent/pricing`: new `priceModel()` returns `{ costUsd, priced }` so an unknown model id is observable instead of silently metering as `$0`. `estimateCost()` is unchanged and now delegates to `priceModel`.
- `@eks-agent/core`: `CallEvent` gains an optional `unpriced` flag, set by the SDK when a model id has no pricing entry, so cost dashboards can surface unmetered traffic.
