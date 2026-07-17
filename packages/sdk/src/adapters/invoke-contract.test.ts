import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import type { MessagesParams } from '../types.js';

import { AnthropicBedrockAdapter } from './anthropic.js';
import { NovaBedrockAdapter } from './nova.js';

// Contract tests pinning the Bedrock InvokeModel response wire shapes the SDK
// adapters parse. The fixtures are recorded, representative InvokeModel response
// bodies (Anthropic Messages + Amazon Nova) — the exact JSON Bedrock returns.
// A test subclass exposes the protected parser and a fake client feeds it the
// fixture bytes, so an upstream field rename (content[].text, usage.input_tokens,
// output.message.content[].text, …) surfaces here rather than as a silently-zero
// token count or empty completion in production.

function fixture(name: string): Uint8Array {
  const path = fileURLToPath(new URL(`./__fixtures__/${name}`, import.meta.url));
  // eslint-disable-next-line security/detect-non-literal-fs-filename -- test-local path resolved from a fixture name relative to this module, not user input
  return readFileSync(path);
}

class ExposedAnthropic extends AnthropicBedrockAdapter {
  setBody(bytes: Uint8Array): void {
    this.client = { send: () => Promise.resolve({ body: bytes }) } as unknown as typeof this.client;
  }
}
class ExposedNova extends NovaBedrockAdapter {
  setBody(bytes: Uint8Array): void {
    this.client = { send: () => Promise.resolve({ body: bytes }) } as unknown as typeof this.client;
  }
}

const params = (modelId: string, modelFamily: MessagesParams['modelFamily']): MessagesParams => ({
  modelId,
  modelFamily,
  messages: [{ role: 'user', content: 'hi' }],
  maxTokens: 64,
  correlationId: 'cid-contract',
});

describe('Bedrock InvokeModel response contract', () => {
  it('parses the recorded Anthropic Messages response, including cache tokens', async () => {
    const a = new ExposedAnthropic({ region: 'us-west-2' });
    a.setBody(fixture('bedrock-invoke-anthropic.json'));

    const res = await a.messages(params('anthropic.claude-sonnet-4-6-20260501-v1:0', 'anthropic'));

    expect(res.text).toBe('The mitochondrion is the powerhouse of the cell.');
    expect(res.stopReason).toBe('end_turn');
    expect(res.usage.inputTokens).toBe(27);
    expect(res.usage.outputTokens).toBe(12);
    // The cache-token fields are load-bearing for prompt-caching cost math.
    expect(res.usage.cacheReadTokens).toBe(1024);
    expect(res.usage.cacheWriteTokens).toBe(512);
  });

  it('parses the recorded Amazon Nova response', async () => {
    const n = new ExposedNova({ region: 'us-west-2' });
    n.setBody(fixture('bedrock-invoke-nova.json'));

    const res = await n.messages(params('amazon.nova-pro-v1:0', 'amazon-nova'));

    expect(res.text).toBe('Amazon Nova is a family of foundation models.');
    expect(res.stopReason).toBe('end_turn');
    expect(res.usage.inputTokens).toBe(20);
    expect(res.usage.outputTokens).toBe(9);
  });
});
