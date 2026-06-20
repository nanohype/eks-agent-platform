# ADR 0002 — Bedrock-only model plane in v1

## Status

Accepted (2026-05-15).

## Context

Whether to ship a multi-provider adapter family (Azure OpenAI, direct-Anthropic, Vertex, Bedrock) on day one, or stay Bedrock-only and add other providers later behind the same interface.

## Decision

v1 ships only `BedrockAdapter` with per-family submodules (Anthropic, Meta, Mistral, Cohere, Titan, Nova, Stability). No direct-API adapters, no Azure OpenAI, no Vertex.

## Why

1. The repo is AWS-native by name and design. Bedrock is the model plane this platform commits to.
2. Bedrock's cross-region inference profiles + Guardrails + invocation logging are first-class. Re-implementing those upstream of Bedrock would be a multi-month detour.
3. Adding a non-Bedrock adapter later is a new `ProviderAdapter` implementation — interface unchanged, error taxonomy unchanged, telemetry attributes unchanged. It is not an architecture change.

## Consequences

- The SDK has a thin family of adapters that share `BedrockAdapter` as a base. Adding a model family means adding `buildRequestBody` + `parseResponseBody` for that family's wire shape, nothing more.
- Pricing tables live in `@eks-agent/pricing`, are Bedrock-only, and are hand-curated — Renovate bumps the package's deps, not the `PRICES` content. An automated refresh from the AWS Pricing API (`scripts/refresh-pricing.mjs`) is Phase-2 and currently a fail-loud scaffold; until it lands, prices are updated by hand, and a model id missing from the table meters as an unmetered `0` (`priced:false` via `priceModel`) rather than a silent real `$0`.
- The error taxonomy in `@eks-agent/core` is provider-agnostic, so the day we add a non-Bedrock adapter, downstream code doesn't change.
