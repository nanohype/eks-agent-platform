import { describe, expect, it } from 'vitest';

import type { MessagesParams } from '../types.js';

import { AnthropicBedrockAdapter } from './anthropic.js';

// Wire-shape + pricing tests for the prompt-cache (cachePoint) surface. A test
// subclass exposes the protected request builder and injects a fake client so
// the cache marker is asserted in the exact place Bedrock reads it, and the
// cache-read/cache-write token accounting is asserted through the real
// messages() → priceModel() path.
class TestAdapter extends AnthropicBedrockAdapter {
  build(params: MessagesParams) {
    return this.buildRequestBody(params);
  }
  setSend(fn: () => Promise<unknown>): void {
    this.client = { send: fn } as unknown as typeof this.client;
  }
}

const base: MessagesParams = {
  modelId: 'anthropic.claude-sonnet-4-6',
  modelFamily: 'anthropic',
  messages: [{ role: 'user', content: 'hi' }],
  maxTokens: 64,
  correlationId: 'cid-cache',
};

describe('cachePoint request wire shape', () => {
  it('leaves the system field a plain string when nothing is cached', () => {
    const adapter = new TestAdapter({ region: 'us-west-2' });
    const body = adapter.build({
      ...base,
      messages: [
        { role: 'system', content: 'be terse' },
        { role: 'user', content: 'hi' },
      ],
    });
    expect(body.system).toBe('be terse');
  });

  it('renders a cache_control breakpoint on the cached system prefix', () => {
    const adapter = new TestAdapter({ region: 'us-west-2' });
    const body = adapter.build({
      ...base,
      messages: [
        { role: 'system', content: 'large stable instructions', cache: true },
        { role: 'user', content: 'the per-request question' },
      ],
    });
    // The stable prefix becomes a one-block array carrying the ephemeral
    // cache_control marker — the InvokeModel form of Bedrock's cachePoint.
    expect(body.system).toEqual([
      { type: 'text', text: 'large stable instructions', cache_control: { type: 'ephemeral' } },
    ]);
    // The per-request user turn stays a plain string — no breakpoint in front
    // of the volatile tail.
    expect(body.messages).toEqual([{ role: 'user', content: 'the per-request question' }]);
  });

  it('places a breakpoint at the end of a cached context message', () => {
    const adapter = new TestAdapter({ region: 'us-west-2' });
    const body = adapter.build({
      ...base,
      messages: [
        { role: 'user', content: 'retrieved corpus …', cache: true },
        { role: 'user', content: 'answer using the corpus' },
      ],
    });
    expect(body.messages).toEqual([
      {
        role: 'user',
        content: [
          { type: 'text', text: 'retrieved corpus …', cache_control: { type: 'ephemeral' } },
        ],
      },
      { role: 'user', content: 'answer using the corpus' },
    ]);
  });
});

function anthropicBody(usage: Record<string, number>): Uint8Array {
  return new TextEncoder().encode(
    JSON.stringify({
      content: [{ type: 'text', text: 'ok' }],
      stop_reason: 'end_turn',
      usage,
    }),
  );
}

describe('cache-token pricing math', () => {
  // claude-sonnet-4-6: input 3.0, output 15.0, cache-read 0.30, cache-write
  // 3.75 per million (packages/pricing/src/data/bedrock-pricing.json).
  it('bills cache-read, cache-write, and regular tokens on their own rates', async () => {
    const adapter = new TestAdapter({ region: 'us-west-2' });
    adapter.setSend(() =>
      Promise.resolve({
        body: anthropicBody({
          input_tokens: 1_000_000,
          output_tokens: 1_000_000,
          cache_read_input_tokens: 1_000_000,
          cache_creation_input_tokens: 1_000_000,
        }),
      }),
    );

    const res = await adapter.messages(base);

    expect(res.usage.cacheReadTokens).toBe(1_000_000);
    expect(res.usage.cacheWriteTokens).toBe(1_000_000);
    // 3.0 + 15.0 + 0.30 + 3.75
    expect(res.costUsd).toBeCloseTo(22.05, 4);
  });

  it('prices a cache-read token far below a full input token', async () => {
    const adapter = new TestAdapter({ region: 'us-west-2' });
    adapter.setSend(() =>
      Promise.resolve({
        body: anthropicBody({
          input_tokens: 0,
          output_tokens: 0,
          cache_read_input_tokens: 1_000_000,
          cache_creation_input_tokens: 0,
        }),
      }),
    );

    const res = await adapter.messages(base);

    // Cache read is 0.30/M vs 3.0/M for a fresh input token — the whole point
    // of the stable-prefix idiom.
    expect(res.costUsd).toBeCloseTo(0.3, 4);
  });
});
