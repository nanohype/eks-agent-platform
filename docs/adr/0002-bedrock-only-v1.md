# ADR 0002 — Bedrock-only model plane, reached through the gateway

## Status

Accepted (2026-05-15).

## Context

Two questions, usually confused for one. Which model providers the platform commits to, and where in the stack an application's model call is mediated.

The second decides the first. A platform that mediates model calls in a library it ships must publish an adapter per provider and per family, and must get every application to import it. A platform that mediates them at a network hop can treat the provider as configuration.

## Decision

The model plane is Bedrock, and it is reached through the tenant's `ModelGateway`. There is no in-process provider adapter and no client library an application imports to call a model.

An application holds a route name and a base URL, and speaks the route's wire format over ordinary HTTP. The `ModelGateway` CR maps each route to a Bedrock foundation model, an inference profile, or an open-weight model imported through Custom Model Import. `spec.routes[].api` fixes the wire format; the resolved format and its base URL are published on `status.routes[]`.

## Why

1. **The gateway is the only place all three guarantees hold at once.** It holds the AWS identity, applies the route's guardrail and rate limit, and records the request. A library cannot enforce any of them — an application that skips the import skips all three, and nothing in the system notices.

2. **Enforcement is a network property here, not a convention.** `gatewayEgressCiliumRules` grants outbound TLS to the gateway's Envoy pods alone. Every other pod in the tenant namespace has no route to a model at all. That is a boundary that holds without asking applications to cooperate.

3. **It makes the model a configuration value.** Repointing a route at a different model is a CR edit with no application change and no redeploy. An adapter-per-family library inverts this: the model becomes a code dependency, and swapping one means a release.

4. **Bedrock's cross-region inference profiles, Guardrails, and invocation logging are first-class.** Re-implementing those upstream of Bedrock would be a multi-month detour for a platform that is AWS-native by name and design.

## Consequences

- **Model families are not a code concern.** Adding one is a route on a CR. Nothing in the repo grows a family-shaped seam for it.
- **The wire format is a route property, and a real constraint.** A route declared `Anthropic` keeps the model's own shape end to end — thinking blocks, cache points, tool use — and is limited to Anthropic-family foundation models. A route declared `OpenAI` reaches every family and stays repointable across them. Pinning it explicitly is what keeps a model swap free; deriving it leaves the route pinned to whatever family it started on.
- **Pricing stays per-family, because cost accounting needs it whatever the wire format was.** A single JSON source of truth (`packages/pricing/src/data/bedrock-pricing.json`) feeds `@eks-agent/pricing` and generates the Lambda cost-publisher table, with a CI drift gate. `scripts/refresh-pricing.mjs` refreshes it from the AWS Price List Query API. A model id missing from the table meters as an unmetered `0` (`priced:false`), surfaced as `unpriced` traffic rather than a silent real `$0`.
- **The error taxonomy in `@eks-agent/core` is provider-agnostic**, so classification does not change with the route's model or format.
- **Failover across models is not offered.** A gateway route resolves to one model. Code that walked an ordered chain of models on a retryable error would have to sit in the application, on the wrong side of the boundary — so if failover is wanted it belongs on the route, not in a client.
