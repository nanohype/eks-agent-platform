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
