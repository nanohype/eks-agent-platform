import { describe, expect, it } from 'vitest';

import type { MessagesParams } from '../types.js';

import { NovaBedrockAdapter } from './nova.js';

// Subclass exposes the protected wire-shape helpers for direct testing.
class TestAdapter extends NovaBedrockAdapter {
  build(params: MessagesParams) {
    return this.buildRequestBody(params);
  }
  parse(body: unknown) {
    return this.parseResponseBody(body);
  }
}

const adapter = new TestAdapter({ region: 'us-west-2' });

const baseParams: MessagesParams = {
  modelId: 'amazon.nova-pro-v1:0',
  modelFamily: 'amazon-nova',
  messages: [{ role: 'user', content: 'hi' }],
  maxTokens: 64,
  correlationId: 'test-cid',
};

describe('NovaBedrockAdapter.buildRequestBody', () => {
  it("produces the Nova 'messages-v1' shape", () => {
    const body = adapter.build(baseParams);
    expect(body.schemaVersion).toBe('messages-v1');
    expect(body.messages).toEqual([{ role: 'user', content: [{ text: 'hi' }] }]);
    expect(body).not.toHaveProperty('system');
  });

  it('hoists system messages into the top-level system array', () => {
    const body = adapter.build({
      ...baseParams,
      messages: [
        { role: 'system', content: 'be terse' },
        { role: 'user', content: 'hi' },
      ],
    });
    expect(body.system).toEqual([{ text: 'be terse' }]);
    expect(body.messages).toEqual([{ role: 'user', content: [{ text: 'hi' }] }]);
  });

  it('keeps multiple system messages as separate array entries (Nova-specific shape)', () => {
    const body = adapter.build({
      ...baseParams,
      messages: [
        { role: 'system', content: 'rule A' },
        { role: 'system', content: 'rule B' },
        { role: 'user', content: 'hi' },
      ],
    });
    expect(body.system).toEqual([{ text: 'rule A' }, { text: 'rule B' }]);
  });

  it('sets inferenceConfig with maxTokens, temperature, stopSequences', () => {
    const body = adapter.build({ ...baseParams, temperature: 0.2, stop: ['STOP'] });
    expect(body.inferenceConfig).toEqual({
      maxTokens: 64,
      temperature: 0.2,
      stopSequences: ['STOP'],
    });
  });

  it('omits temperature + stopSequences when not provided', () => {
    const body = adapter.build(baseParams);
    expect(body.inferenceConfig).toEqual({ maxTokens: 64 });
  });
});

describe('NovaBedrockAdapter.parseResponseBody', () => {
  it('extracts text content and usage', () => {
    const parsed = adapter.parse({
      output: { message: { content: [{ text: 'pong' }] } },
      stopReason: 'end_turn',
      usage: { inputTokens: 10, outputTokens: 2 },
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

  it('concatenates multi-segment Nova content', () => {
    const parsed = adapter.parse({
      output: { message: { content: [{ text: 'a-' }, { text: 'b' }] } },
      stopReason: 'end_turn',
      usage: { inputTokens: 1, outputTokens: 1 },
    });
    expect(parsed.text).toBe('a-b');
  });

  it("defaults stopReason to 'end_turn' when absent", () => {
    const parsed = adapter.parse({
      output: { message: { content: [{ text: 'x' }] } },
      usage: { inputTokens: 1, outputTokens: 1 },
    });
    expect(parsed.stopReason).toBe('end_turn');
  });

  it('returns empty text when output.message is missing', () => {
    const parsed = adapter.parse({
      stopReason: 'end_turn',
      usage: { inputTokens: 1, outputTokens: 1 },
    });
    expect(parsed.text).toBe('');
  });

  it('reports zero usage when usage block is missing entirely', () => {
    const parsed = adapter.parse({
      output: { message: { content: [{ text: 'x' }] } },
      stopReason: 'end_turn',
    });
    expect(parsed.usage).toEqual({
      inputTokens: 0,
      outputTokens: 0,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
    });
  });
});
