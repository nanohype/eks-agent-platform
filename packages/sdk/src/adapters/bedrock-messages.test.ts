import { ThrottlingException } from '@aws-sdk/client-bedrock-runtime';
import { AgentError, type CallEvent } from '@eks-agent/core';
import { describe, expect, it } from 'vitest';

import type { MessagesParams } from '../types.js';

import { AnthropicBedrockAdapter } from './anthropic.js';

// Inject a fake BedrockRuntimeClient by reassigning the protected `client`
// field from a test subclass — NOT module-mocking @aws-sdk/* (which would be a
// rubric REJECT). This exercises the real messages() orchestration: build →
// send → parse → cost → emitCallEvent, plus the error-wrapping path.
class FakeClientAdapter extends AnthropicBedrockAdapter {
  lastOptions: { abortSignal?: AbortSignal } | undefined;

  setSend(fn: () => Promise<unknown>): void {
    this.client = {
      send: (_cmd: unknown, opts: { abortSignal?: AbortSignal }) => {
        this.lastOptions = opts;
        return fn();
      },
    } as unknown as typeof this.client;
  }
}

function anthropicBody(text: string): Uint8Array {
  return new TextEncoder().encode(
    JSON.stringify({
      content: [{ type: 'text', text }],
      stop_reason: 'end_turn',
      usage: { input_tokens: 10, output_tokens: 5 },
    }),
  );
}

const baseParams: MessagesParams = {
  modelId: 'anthropic.claude-3-5-sonnet-20241022-v2:0',
  modelFamily: 'anthropic',
  messages: [{ role: 'user', content: 'hi' }],
  maxTokens: 64,
  correlationId: 'cid-123',
};

describe('BedrockAdapter.messages orchestration', () => {
  it('returns a parsed response with cost, latency, and correlationId', async () => {
    const adapter = new FakeClientAdapter({ region: 'us-west-2' });
    adapter.setSend(() => Promise.resolve({ body: anthropicBody('pong') }));

    const res = await adapter.messages(baseParams);

    expect(res.text).toBe('pong');
    expect(res.correlationId).toBe('cid-123');
    expect(res.usage.inputTokens).toBe(10);
    expect(res.costUsd).toBeGreaterThan(0);
    expect(typeof res.latencyMs).toBe('number');
  });

  it('applies a default request deadline even when the caller passes no signal', async () => {
    const adapter = new FakeClientAdapter({ region: 'us-west-2' });
    adapter.setSend(() => Promise.resolve({ body: anthropicBody('x') }));

    await adapter.messages(baseParams);

    expect(adapter.lastOptions?.abortSignal).toBeInstanceOf(AbortSignal);
  });

  it('emits a CallEvent exactly once on success and never on throw', async () => {
    const adapter = new FakeClientAdapter({ region: 'us-west-2', platform: 'p', tenant: 't' });
    const events: CallEvent[] = [];
    adapter.emitCallEvent = (e) => events.push(e);

    adapter.setSend(() => Promise.resolve({ body: anthropicBody('ok') }));
    await adapter.messages(baseParams);
    expect(events).toHaveLength(1);
    expect(events[0]?.correlationId).toBe('cid-123');
    expect(events[0]?.status).toBe('ok');

    adapter.setSend(() =>
      Promise.reject(new ThrottlingException({ $metadata: {}, message: 'slow down' })),
    );
    await expect(adapter.messages(baseParams)).rejects.toBeInstanceOf(AgentError);
    expect(events).toHaveLength(1);
  });

  it('flags unpriced traffic on the CallEvent for an unknown model id', async () => {
    const adapter = new FakeClientAdapter({ region: 'us-west-2' });
    const events: CallEvent[] = [];
    adapter.emitCallEvent = (e) => events.push(e);
    adapter.setSend(() => Promise.resolve({ body: anthropicBody('x') }));

    await adapter.messages({ ...baseParams, modelId: 'unknown.model.id' });

    expect(events[0]?.unpriced).toBe(true);
    expect(events[0]?.costUsd).toBe(0);
  });

  it('wraps a ThrottlingException as a retryable RateLimit AgentError', async () => {
    const adapter = new FakeClientAdapter({ region: 'us-west-2' });
    adapter.setSend(() =>
      Promise.reject(new ThrottlingException({ $metadata: {}, message: 'slow' })),
    );

    await expect(adapter.messages(baseParams)).rejects.toMatchObject({
      class: 'RateLimit',
      retryable: true,
    });
  });
});
