# ADR 0002 — Bedrock-only model plane in v1

## Status

Accepted (2026-05-15).

## Context

Whether to ship a multi-provider adapter family (Azure OpenAI, direct-Anthropic, Vertex, Bedrock) on day one, or stay Bedrock-only and add other providers later behind the same interface.

## Decision

v1 ships only `BedrockAdapter`, with a family registry in `packages/sdk/src/factory.ts`. Two family adapters are registered — Anthropic and Amazon Nova — each subclassing `BedrockAdapter` with its own request/response wire shape. Any family without a registered adapter loud-fails at construction (`createBedrockAdapter` throws, naming the shipped families) rather than silently mis-routing to a default. No direct-API adapters, no Azure OpenAI, no Vertex.

## Why

1. The repo is AWS-native by name and design. Bedrock is the model plane this platform commits to.
2. Bedrock's cross-region inference profiles + Guardrails + invocation logging are first-class. Re-implementing those upstream of Bedrock would be a multi-month detour.
3. Adding a non-Bedrock adapter later is a new `ProviderAdapter` implementation — interface unchanged, error taxonomy unchanged, telemetry attributes unchanged. It is not an architecture change.

## Consequences

- The SDK has a thin family of adapters that share `BedrockAdapter` as a base. Adding a model family means adding `buildRequestBody` + `parseResponseBody` for that family's wire shape plus a registry insert, nothing more.
- Pricing is Bedrock-only and lives in a single JSON source of truth (`packages/pricing/src/data/bedrock-pricing.json`) that `@eks-agent/pricing` imports and the Lambda cost-publisher table is generated from (with a CI drift gate). `scripts/refresh-pricing.mjs` refreshes that JSON from the AWS Price List Query API (SigV4-signed `pricing:GetProducts`, weekly cadence, `--dry-run` to preview the diff). A model id missing from the table meters as an unmetered `0` (`priced:false` via `priceModel`), surfaced as `unpriced` traffic rather than a silent real `$0`.
- The error taxonomy in `@eks-agent/core` is provider-agnostic, so the day we add a non-Bedrock adapter, downstream code doesn't change.
