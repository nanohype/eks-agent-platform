# @eks-agent/sdk

Provider-agnostic call surface for Bedrock-hosted LLMs. Every model family ships its own adapter with the same call shape, the same error taxonomy, and the same telemetry attributes — switching models is a config change, not a code change.

## Adapters shipped

| Adapter                   | Module                 | Notes                                                                |
| ------------------------- | ---------------------- | -------------------------------------------------------------------- |
| `AnthropicBedrockAdapter` | `./adapters/anthropic` | Claude 3 / 3.5 / 4 family on Bedrock; supports prompt caching tokens |
| `NovaBedrockAdapter`      | `./adapters/nova`      | Amazon Nova Pro / Lite / Micro                                       |
| `MetaBedrockAdapter`      | _(planned)_            | Llama 3.x                                                            |
| `MistralBedrockAdapter`   | _(planned)_            | Mistral Large / Small                                                |
| `CohereBedrockAdapter`    | _(planned)_            | Command R / R+                                                       |
| `TitanBedrockAdapter`     | _(planned)_            | Embeddings + Titan text                                              |
| `StabilityBedrockAdapter` | _(planned)_            | Image gen                                                            |

## Usage

```ts
import { AnthropicBedrockAdapter, type ProviderAdapter } from '@eks-agent/sdk';
import { CorrelationId } from '@eks-agent/core';

const claude: ProviderAdapter = new AnthropicBedrockAdapter({
  region: process.env.AWS_REGION!,
  // credentials default to fromNodeProviderChain (= IRSA inside the pod)
});

const result = await claude.messages({
  modelId: 'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
  modelFamily: 'anthropic',
  messages: [{ role: 'user', content: 'Reply with pong.' }],
  maxTokens: 64,
  correlationId: CorrelationId(),
});
console.log(result.text, result.costUsd);
```

Every call:

- Authenticates via the pod's IRSA role (no API keys)
- Estimates cost via `@eks-agent/pricing`
- Returns a unified `MessagesResponse` with `stopReason`, `usage`, `costUsd`, `latencyMs`
- Throws `AgentError` (from `@eks-agent/core`) with `classifyError()` mapped to the unified `ErrorClass` enum

## Prompt caching (cachePoint)

`llm-policy` mandates prompt caching. Mark a cache breakpoint by setting `cache: true` on a message; on the Anthropic families this renders as a `cache_control: {type: 'ephemeral'}` block (the InvokeModel form of Bedrock's Converse `cachePoint`).

**The stable-prefix idiom.** Put the unchanging part of the prompt FIRST — the system instructions and any large reused context (tool schemas, a retrieved corpus, few-shot examples) — and mark the LAST stable message with `cache: true`. Bedrock caches every input token up to and including that breakpoint; a later call with the same prefix reads it back at the cache-read rate (~10× cheaper than a fresh input token) instead of re-billing. Only put content that repeats verbatim across calls before the breakpoint — a breakpoint in front of the per-request tail never hits.

```ts
const result = await claude.messages({
  modelId: 'anthropic.claude-sonnet-4-6',
  modelFamily: 'anthropic',
  messages: [
    { role: 'system', content: LARGE_STABLE_SYSTEM_PROMPT, cache: true }, // ← cached prefix
    { role: 'user', content: perRequestQuestion }, // ← volatile tail, uncached
  ],
  maxTokens: 512,
  correlationId: CorrelationId(),
});
// result.usage.cacheReadTokens / cacheWriteTokens feed the cost math in @eks-agent/pricing.
```

## Streaming

`messagesStream` is the streaming counterpart of `messages` — same auth, request-deadline, error taxonomy, and cost accounting. Text deltas surface via `onText`; the resolved `MessagesResponse` still carries the full text, final usage, and cost. The cost event (when `emitCallEvent` is wired) fires once, after the stream terminates and the full token usage is known.

```ts
const res = await claude.messagesStream(
  {
    modelId: 'anthropic.claude-sonnet-4-6',
    modelFamily: 'anthropic',
    messages,
    maxTokens: 512,
    correlationId: CorrelationId(),
  },
  { onText: (delta) => process.stdout.write(delta) },
);
console.log('\n', res.usage, res.costUsd);
```

A mid-stream failure (a modeled `ResponseStream` error member or a thrown exception) is wrapped as an `AgentError` with the same `classifyError()` mapping as a synchronous call, and no success cost event is emitted.

## Model fallback router

`createModelRouter` walks an ordered `[primary, fallback, …]` chain. Each rung is tried in turn; on a **non-terminal** failure (throttling, overload, server, network, or a model unavailable in-region) it emits an error `CallEvent` for that rung — so cost/usage accounting sees every attempt, not only the one that answered — and advances to the next model. It resolves with the first rung that answers, or throws `ChainExhaustedError` (carrying every per-rung error) when the whole chain fails. `Cancelled`, `BudgetExceeded`, and `GuardrailBlock` are terminal — rethrown as-is, never routed around.

```ts
import { createModelRouter } from '@eks-agent/sdk';

const router = createModelRouter(
  [
    { modelFamily: 'anthropic', modelId: 'anthropic.claude-sonnet-4-6' }, // primary
    { modelFamily: 'anthropic', modelId: 'anthropic.claude-haiku-4-5-20251001-v1:0' }, // fallback
  ],
  { region: process.env.AWS_REGION!, platform: 'my-app', tenant: 'my-team', onCallEvent: emit },
);

const res = await router.messages({ messages, maxTokens: 512, correlationId: CorrelationId() });
```
