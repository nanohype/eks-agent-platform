import { AgentError, type CallEvent, type ModelFamily } from '@eks-agent/core';
import { describe, expect, it } from 'vitest';

import type { BedrockAdapter, BedrockAdapterOptions } from './adapters/bedrock-base.js';
import { ChainExhaustedError, createModelRouter, type RouteTarget } from './factory.js';
import type { MessagesParams, MessagesResponse } from './types.js';

// The router is exercised with injected fake adapters (the `adapterFor` seam),
// so no AWS calls happen. A per-modelId plan map drives each rung to succeed or
// throw; the fake mirrors a real adapter's telemetry contract — it emits an
// `ok` CallEvent on success and throws (no event) on failure, leaving the
// error event to the router.

type Plan = { kind: 'ok' } | { kind: 'throw'; err: AgentError };

function okResponse(modelId: string): MessagesResponse {
  return {
    text: `answer from ${modelId}`,
    stopReason: 'end_turn',
    usage: { inputTokens: 5, outputTokens: 5, cacheReadTokens: 0, cacheWriteTokens: 0 },
    costUsd: 0.001,
    latencyMs: 1,
    correlationId: 'cid-router',
  };
}

class FakeAdapter {
  emitCallEvent?: (e: CallEvent) => void;
  constructor(
    readonly modelFamily: ModelFamily,
    private readonly plans: Map<string, Plan>,
    private readonly opts: BedrockAdapterOptions,
  ) {}

  messages(p: MessagesParams): Promise<MessagesResponse> {
    const plan = this.plans.get(p.modelId) ?? { kind: 'ok' };
    if (plan.kind === 'throw') return Promise.reject(plan.err);
    const res = okResponse(p.modelId);
    this.emitCallEvent?.({
      correlationId: p.correlationId,
      platform: this.opts.platform ?? '',
      tenant: this.opts.tenant ?? '',
      modelFamily: this.modelFamily,
      modelId: p.modelId,
      tokens: res.usage,
      costUsd: res.costUsd,
      latencyMs: res.latencyMs,
      status: 'ok',
      timestamp: new Date().toISOString(),
    });
    return Promise.resolve(res);
  }
}

function adapterForPlans(plans: Map<string, Plan>) {
  return (family: ModelFamily, opts: BedrockAdapterOptions): BedrockAdapter =>
    new FakeAdapter(family, plans, opts) as unknown as BedrockAdapter;
}

const callParams = {
  messages: [{ role: 'user' as const, content: 'hi' }],
  maxTokens: 64,
  correlationId: 'cid-router',
};

const sonnet: RouteTarget = { modelFamily: 'anthropic', modelId: 'anthropic.claude-sonnet-4-6' };
const haiku: RouteTarget = {
  modelFamily: 'anthropic',
  modelId: 'anthropic.claude-haiku-4-5-20251001-v1:0',
};
const nova: RouteTarget = { modelFamily: 'amazon-nova', modelId: 'amazon.nova-pro-v1:0' };

describe('createModelRouter', () => {
  it('returns the primary response and emits one ok event when the primary answers', async () => {
    const events: CallEvent[] = [];
    const router = createModelRouter([sonnet, nova], {
      region: 'us-west-2',
      onCallEvent: (e) => events.push(e),
      adapterFor: adapterForPlans(new Map()),
    });

    const res = await router.messages(callParams);

    expect(res.text).toBe('answer from anthropic.claude-sonnet-4-6');
    expect(events).toHaveLength(1);
    expect(events[0]?.status).toBe('ok');
    expect(events[0]?.modelId).toBe(sonnet.modelId);
  });

  it('falls back on a non-terminal failure and emits an event per attempt', async () => {
    const events: CallEvent[] = [];
    const plans = new Map<string, Plan>([
      [
        sonnet.modelId,
        { kind: 'throw', err: new AgentError({ class: 'Overloaded', message: 'busy' }) },
      ],
    ]);
    const router = createModelRouter([sonnet, nova], {
      region: 'us-west-2',
      platform: 'app',
      tenant: 'team',
      onCallEvent: (e) => events.push(e),
      adapterFor: adapterForPlans(plans),
    });

    const res = await router.messages(callParams);

    expect(res.text).toBe('answer from amazon.nova-pro-v1:0');
    // One error event for the failed primary, one ok event for the fallback.
    expect(events).toHaveLength(2);
    expect(events[0]).toMatchObject({
      status: 'error',
      errorClass: 'Overloaded',
      modelId: sonnet.modelId,
      costUsd: 0,
    });
    expect(events[0]?.tokens).toEqual({
      inputTokens: 0,
      outputTokens: 0,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
    });
    expect(events[1]).toMatchObject({ status: 'ok', modelId: nova.modelId });
  });

  it('falls back within a single family (sonnet → haiku on the same adapter)', async () => {
    const plans = new Map<string, Plan>([
      [sonnet.modelId, { kind: 'throw', err: new AgentError({ class: 'Server', message: '5xx' }) }],
    ]);
    const router = createModelRouter([sonnet, haiku], {
      region: 'us-west-2',
      adapterFor: adapterForPlans(plans),
    });

    const res = await router.messages(callParams);
    expect(res.text).toBe(`answer from ${haiku.modelId}`);
  });

  it('throws ChainExhaustedError with every attempt when the whole chain fails', async () => {
    const events: CallEvent[] = [];
    const plans = new Map<string, Plan>([
      [
        sonnet.modelId,
        { kind: 'throw', err: new AgentError({ class: 'Overloaded', message: 'busy' }) },
      ],
      [nova.modelId, { kind: 'throw', err: new AgentError({ class: 'Server', message: '5xx' }) }],
    ]);
    const router = createModelRouter([sonnet, nova], {
      region: 'us-west-2',
      onCallEvent: (e) => events.push(e),
      adapterFor: adapterForPlans(plans),
    });

    await expect(router.messages(callParams)).rejects.toBeInstanceOf(ChainExhaustedError);
    // Both rungs emitted an error event before exhaustion.
    expect(events.filter((e) => e.status === 'error')).toHaveLength(2);

    const err = await router.messages(callParams).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ChainExhaustedError);
    const exhausted = err as ChainExhaustedError;
    expect(exhausted.attempts).toHaveLength(2);
    expect(exhausted.attempts[0]?.target.modelId).toBe(sonnet.modelId);
    expect(exhausted.attempts[1]?.error.class).toBe('Server');
    expect(exhausted.message).toContain('chain exhausted');
  });

  it.each(['Cancelled', 'BudgetExceeded', 'GuardrailBlock'] as const)(
    'never spends the fallback on a terminal %s error',
    async (cls) => {
      const events: CallEvent[] = [];
      const plans = new Map<string, Plan>([
        [sonnet.modelId, { kind: 'throw', err: new AgentError({ class: cls, message: cls }) }],
      ]);
      const router = createModelRouter([sonnet, nova], {
        region: 'us-west-2',
        onCallEvent: (e) => events.push(e),
        adapterFor: adapterForPlans(plans),
      });

      const err = await router.messages(callParams).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(AgentError);
      expect((err as AgentError).class).toBe(cls);
      // Terminal errors are rethrown as-is — never wrapped in ChainExhausted,
      // never emitted as a router error event, never routed to the fallback.
      expect(err).not.toBeInstanceOf(ChainExhaustedError);
      expect(events).toHaveLength(0);
    },
  );

  it('wraps a non-AgentError thrown by an adapter as a Server failure', async () => {
    const plans = new Map<string, Plan>([
      [sonnet.modelId, { kind: 'throw', err: new TypeError('boom') as unknown as AgentError }],
    ]);
    const router = createModelRouter([sonnet, nova], {
      region: 'us-west-2',
      adapterFor: adapterForPlans(plans),
    });
    const res = await router.messages(callParams);
    // Non-AgentError from the primary is treated as a Server failure (non-terminal) → fallback.
    expect(res.text).toBe(`answer from ${nova.modelId}`);
  });

  it('rejects an empty chain', () => {
    expect(() => createModelRouter([], { region: 'us-west-2' })).toThrow(
      /at least one RouteTarget/,
    );
  });

  it('builds real adapters by default and rejects an unshipped family', () => {
    expect(() =>
      createModelRouter([{ modelFamily: 'meta', modelId: 'meta.llama' }], { region: 'us-west-2' }),
    ).toThrow(/no BedrockAdapter registered/);
  });
});
