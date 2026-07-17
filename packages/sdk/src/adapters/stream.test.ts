import { ModelStreamErrorException, ThrottlingException } from '@aws-sdk/client-bedrock-runtime';
import { AgentError, type CallEvent } from '@eks-agent/core';
import { describe, expect, it } from 'vitest';

import type { MessagesParams } from '../types.js';

import { AnthropicBedrockAdapter } from './anthropic.js';
import { NovaBedrockAdapter } from './nova.js';

// Streaming-adapter tests. A fake BedrockRuntimeClient returns a body that is
// an async iterable of ResponseStream-shaped members — the same shape
// InvokeModelWithResponseStream yields — so the accumulate → price → emit path
// runs for real without touching AWS. Error members and a throwing iterator
// both exercise the mid-stream failure path.

function chunk(obj: unknown): unknown {
  return { chunk: { bytes: new TextEncoder().encode(JSON.stringify(obj)) } };
}

// Sync generators are valid `for await` sources; the production body type is
// AsyncIterable, but at runtime for-await consumes either. Kept sync so there's
// no vacuous `await` just to satisfy the async-generator shape.
function* streamOf(items: unknown[]): Iterable<unknown> {
  for (const it of items) yield it;
}

function* streamThenThrow(items: unknown[], err: unknown): Iterable<unknown> {
  for (const it of items) yield it;
  throw err;
}

class FakeAnthropic extends AnthropicBedrockAdapter {
  setBody(body: unknown): void {
    this.client = { send: () => Promise.resolve({ body }) } as unknown as typeof this.client;
  }
}
class FakeNova extends NovaBedrockAdapter {
  setBody(body: unknown): void {
    this.client = { send: () => Promise.resolve({ body }) } as unknown as typeof this.client;
  }
}

const params: MessagesParams = {
  modelId: 'anthropic.claude-sonnet-4-6',
  modelFamily: 'anthropic',
  messages: [{ role: 'user', content: 'stream please' }],
  maxTokens: 64,
  correlationId: 'cid-stream',
};

const anthropicHappyPath = [
  {
    type: 'message_start',
    message: {
      usage: { input_tokens: 10, cache_read_input_tokens: 100, cache_creation_input_tokens: 50 },
    },
  },
  { type: 'content_block_delta', delta: { type: 'text_delta', text: 'po' } },
  { type: 'content_block_delta', delta: { type: 'text_delta', text: 'ng' } },
  { type: 'message_delta', delta: { stop_reason: 'end_turn' }, usage: { output_tokens: 5 } },
  { type: 'message_stop' },
];

describe('AnthropicBedrockAdapter.messagesStream', () => {
  it('accumulates deltas, final usage, cost, and stop reason on clean completion', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(streamOf(anthropicHappyPath.map(chunk)));

    const deltas: string[] = [];
    const res = await adapter.messagesStream(params, { onText: (d) => deltas.push(d) });

    expect(deltas).toEqual(['po', 'ng']);
    expect(res.text).toBe('pong');
    expect(res.stopReason).toBe('end_turn');
    expect(res.usage.inputTokens).toBe(10);
    expect(res.usage.outputTokens).toBe(5);
    expect(res.usage.cacheReadTokens).toBe(100);
    expect(res.usage.cacheWriteTokens).toBe(50);
    // input 10 + output 5 + cache read 100 + cache write 50, all at sonnet-4-6
    // per-million rates → strictly positive.
    expect(res.costUsd).toBeGreaterThan(0);
    expect(res.correlationId).toBe('cid-stream');
  });

  it('works without an onText handler', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(streamOf(anthropicHappyPath.map(chunk)));
    const res = await adapter.messagesStream(params);
    expect(res.text).toBe('pong');
  });

  it('prefers the final invocation-metrics token counts', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(
      streamOf(
        [
          { type: 'message_start', message: { usage: { input_tokens: 1 } } },
          { type: 'content_block_delta', delta: { type: 'text_delta', text: 'x' } },
          {
            'amazon-bedrock-invocationMetrics': {
              inputTokenCount: 42,
              outputTokenCount: 7,
              cacheReadInputTokenCount: 9,
              cacheWriteInputTokenCount: 3,
            },
          },
        ].map(chunk),
      ),
    );

    const res = await adapter.messagesStream(params);

    expect(res.usage.inputTokens).toBe(42);
    expect(res.usage.outputTokens).toBe(7);
    expect(res.usage.cacheReadTokens).toBe(9);
    expect(res.usage.cacheWriteTokens).toBe(3);
  });

  it('emits a CallEvent exactly once, only after a clean completion', async () => {
    const events: CallEvent[] = [];
    const ok = new FakeAnthropic({ region: 'us-west-2', platform: 'p', tenant: 't' });
    ok.emitCallEvent = (e) => events.push(e);
    ok.setBody(streamOf(anthropicHappyPath.map(chunk)));
    await ok.messagesStream(params);
    expect(events).toHaveLength(1);
    expect(events[0]?.status).toBe('ok');

    // A mid-stream failure must not emit a success event.
    const bad = new FakeAnthropic({ region: 'us-west-2', platform: 'p', tenant: 't' });
    const failEvents: CallEvent[] = [];
    bad.emitCallEvent = (e) => failEvents.push(e);
    bad.setBody(
      streamOf([
        chunk({ type: 'content_block_delta', delta: { type: 'text_delta', text: 'partial' } }),
        {
          modelStreamErrorException: new ModelStreamErrorException({
            $metadata: {},
            message: 'boom',
          }),
        },
      ]),
    );
    await expect(bad.messagesStream(params)).rejects.toBeInstanceOf(AgentError);
    expect(failEvents).toHaveLength(0);
  });

  it('classifies a modeled mid-stream error member', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(
      streamOf([
        chunk({ type: 'content_block_delta', delta: { type: 'text_delta', text: 'hi' } }),
        {
          modelStreamErrorException: new ModelStreamErrorException({
            $metadata: {},
            message: 'mid-stream',
          }),
        },
      ]),
    );
    await expect(adapter.messagesStream(params)).rejects.toMatchObject({ class: 'Server' });
  });

  it('classifies a thrown mid-stream error via the shared taxonomy', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(
      streamThenThrow(
        [chunk({ type: 'content_block_delta', delta: { type: 'text_delta', text: 'hi' } })],
        new ThrottlingException({ $metadata: {}, message: 'slow' }),
      ),
    );
    await expect(adapter.messagesStream(params)).rejects.toMatchObject({
      class: 'RateLimit',
      retryable: true,
    });
  });

  it('ignores an unrecognized stop reason and a usage-less message_start', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(
      streamOf(
        [
          { type: 'message_start', message: {} },
          { type: 'content_block_delta', delta: { type: 'text_delta', text: 'x' } },
          // stop_reason not in the known set → left at the default.
          { type: 'message_delta', delta: { stop_reason: 'weird' }, usage: { output_tokens: 2 } },
        ].map(chunk),
      ),
    );
    const res = await adapter.messagesStream(params);
    expect(res.stopReason).toBe('end_turn');
    expect(res.usage.inputTokens).toBe(0);
    expect(res.usage.outputTokens).toBe(2);
  });

  it('tolerates an empty stream body', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(undefined);
    const res = await adapter.messagesStream(params);
    expect(res.text).toBe('');
    expect(res.usage.outputTokens).toBe(0);
  });

  it('skips chunks that carry no payload bytes', async () => {
    const adapter = new FakeAnthropic({ region: 'us-west-2' });
    adapter.setBody(
      streamOf([
        { chunk: {} },
        chunk({ type: 'content_block_delta', delta: { type: 'text_delta', text: 'ok' } }),
        { somethingElse: true },
      ]),
    );
    const res = await adapter.messagesStream(params);
    expect(res.text).toBe('ok');
  });
});

describe('NovaBedrockAdapter.messagesStream', () => {
  it('accumulates Nova streaming deltas, stop reason, and usage', async () => {
    const adapter = new FakeNova({ region: 'us-west-2' });
    adapter.setBody(
      streamOf(
        [
          { contentBlockDelta: { delta: { text: 'Nova ' } } },
          { contentBlockDelta: { delta: { text: 'streams' } } },
          { messageStop: { stopReason: 'end_turn' } },
          { metadata: { usage: { inputTokens: 12, outputTokens: 4 } } },
        ].map(chunk),
      ),
    );

    const deltas: string[] = [];
    const res = await adapter.messagesStream(
      { ...params, modelId: 'amazon.nova-pro-v1:0', modelFamily: 'amazon-nova' },
      { onText: (d) => deltas.push(d) },
    );

    expect(deltas).toEqual(['Nova ', 'streams']);
    expect(res.text).toBe('Nova streams');
    expect(res.stopReason).toBe('end_turn');
    expect(res.usage.inputTokens).toBe(12);
    expect(res.usage.outputTokens).toBe(4);
  });

  it('leaves the Nova stop reason at the default for an unknown value', async () => {
    const adapter = new FakeNova({ region: 'us-west-2' });
    adapter.setBody(
      streamOf(
        [
          { contentBlockDelta: { delta: { text: 'hi' } } },
          { messageStop: { stopReason: 'mystery' } },
        ].map(chunk),
      ),
    );
    const res = await adapter.messagesStream({
      ...params,
      modelId: 'amazon.nova-pro-v1:0',
      modelFamily: 'amazon-nova',
    });
    expect(res.stopReason).toBe('end_turn');
    expect(res.text).toBe('hi');
  });
});
