import { describe, expect, it } from 'vitest';

import type { MessagesParams } from '../types.js';

import { AnthropicBedrockAdapter } from './anthropic.js';

// Subclass exposes the protected wire-shape helpers for direct testing.
class TestAdapter extends AnthropicBedrockAdapter {
  build(params: MessagesParams) {
    return this.buildRequestBody(params);
  }
  parse(body: unknown) {
    return this.parseResponseBody(body);
  }
}

const adapter = new TestAdapter({ region: 'us-west-2' });

const baseParams: MessagesParams = {
  modelId: 'anthropic.claude-3-5-sonnet-20241022-v2:0',
  modelFamily: 'anthropic',
  messages: [{ role: 'user', content: 'hi' }],
  maxTokens: 64,
  correlationId: 'test-cid',
};

describe('AnthropicBedrockAdapter.buildRequestBody', () => {
  it('produces the Bedrock-versioned Anthropic Messages shape', () => {
    const body = adapter.build(baseParams);
    expect(body.anthropic_version).toBe('bedrock-2023-05-31');
    expect(body.max_tokens).toBe(64);
    expect(body.messages).toEqual([{ role: 'user', content: 'hi' }]);
    expect(body).not.toHaveProperty('system');
  });

  it('hoists system messages to the top-level system field', () => {
    const body = adapter.build({
      ...baseParams,
      messages: [
        { role: 'system', content: 'be terse' },
        { role: 'user', content: 'hi' },
      ],
    });
    expect(body.system).toBe('be terse');
    expect(body.messages).toEqual([{ role: 'user', content: 'hi' }]);
  });

  it('concatenates multiple system messages with double newline', () => {
    const body = adapter.build({
      ...baseParams,
      messages: [
        { role: 'system', content: 'rule A' },
        { role: 'system', content: 'rule B' },
        { role: 'user', content: 'hi' },
      ],
    });
    expect(body.system).toBe('rule A\n\nrule B');
  });

  it('passes through temperature and stop sequences', () => {
    const body = adapter.build({ ...baseParams, temperature: 0.2, stop: ['STOP'] });
    expect(body.temperature).toBe(0.2);
    expect(body.stop_sequences).toEqual(['STOP']);
  });
});

describe('AnthropicBedrockAdapter.parseResponseBody', () => {
  it('extracts text content and usage', () => {
    const parsed = adapter.parse({
      content: [{ type: 'text', text: 'pong' }],
      stop_reason: 'end_turn',
      usage: { input_tokens: 10, output_tokens: 2 },
    });
    expect(parsed.text).toBe('pong');
    expect(parsed.usage).toEqual({
      inputTokens: 10,
      outputTokens: 2,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
    });
    expect(parsed.stopReason).toBe('end_turn');
  });

  it('concatenates multi-segment text content', () => {
    const parsed = adapter.parse({
      content: [
        { type: 'text', text: 'pong-' },
        { type: 'text', text: 'pong' },
      ],
      stop_reason: 'end_turn',
      usage: { input_tokens: 1, output_tokens: 1 },
    });
    expect(parsed.text).toBe('pong-pong');
  });

  it('captures Anthropic prompt-cache tokens', () => {
    const parsed = adapter.parse({
      content: [{ type: 'text', text: 'x' }],
      stop_reason: 'end_turn',
      usage: {
        input_tokens: 5,
        output_tokens: 5,
        cache_read_input_tokens: 100,
        cache_creation_input_tokens: 50,
      },
    });
    expect(parsed.usage.cacheReadTokens).toBe(100);
    expect(parsed.usage.cacheWriteTokens).toBe(50);
  });

  it('falls back to amazon_bedrock_invocation_metrics when usage is missing', () => {
    const parsed = adapter.parse({
      content: [{ type: 'text', text: 'x' }],
      stop_reason: 'end_turn',
      amazon_bedrock_invocation_metrics: { inputTokenCount: 7, outputTokenCount: 3 },
    });
    expect(parsed.usage.inputTokens).toBe(7);
    expect(parsed.usage.outputTokens).toBe(3);
  });

  it("defaults stopReason to 'end_turn' when absent", () => {
    const parsed = adapter.parse({
      content: [{ type: 'text', text: 'x' }],
      usage: { input_tokens: 1, output_tokens: 1 },
    });
    expect(parsed.stopReason).toBe('end_turn');
  });

  it('returns empty text when content is missing', () => {
    const parsed = adapter.parse({
      stop_reason: 'end_turn',
      usage: { input_tokens: 1, output_tokens: 1 },
    });
    expect(parsed.text).toBe('');
  });
});
