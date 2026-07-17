import { AgentError } from '@eks-agent/core';
import { describe, expect, it, vi } from 'vitest';

import { GatewayBackend, asAgentError, classifyStatus } from './model.js';
import type { GatewayResponse } from './model.js';

function jsonResponse(body: GatewayResponse, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'content-type': 'application/json' },
    }),
  );
}

const inv = { name: 'c', input: 'hi', correlationId: 'cid', signal: AbortSignal.timeout(5000) };

describe('GatewayBackend', () => {
  it('prices a call when the gateway reports modelId + usage', async () => {
    const fetchImpl = vi.fn<typeof fetch>(() =>
      jsonResponse({
        output: 'hello there',
        stopReason: 'end_turn',
        modelId: 'anthropic.claude-sonnet-4-6',
        usage: { inputTokens: 1000, outputTokens: 500 },
      }),
    );
    const be = new GatewayBackend({
      gateway: 'http://gw:8080/',
      platform: 'p',
      fleet: 'f',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(r.output).toBe('hello there');
    expect(r.unpriced).toBe(false);
    // (1000/1e6)*3 + (500/1e6)*15 = 0.0105
    expect(r.costUsd).toBeCloseTo(0.0105, 8);
    expect(r.guardrailBlocked).toBe(false);
    // trailing slash on the base url is normalized away
    expect(fetchImpl.mock.calls[0]?.[0]).toBe('http://gw:8080/v1/agents/p-f/messages');
  });

  it('strips a cross-region prefix before pricing', async () => {
    const fetchImpl = vi.fn(() =>
      jsonResponse({
        output: 'ok',
        modelId: 'us.anthropic.claude-sonnet-4-6',
        usage: { inputTokens: 1000, outputTokens: 0 },
      }),
    );
    const be = new GatewayBackend({ gateway: 'http://gw', platform: 'p', fleet: 'f', fetchImpl });
    const r = await be.invoke(inv);
    expect(r.unpriced).toBe(false);
    expect(r.costUsd).toBeCloseTo(0.003, 8);
  });

  it('marks an unpriced model without pretending $0 is free', async () => {
    const fetchImpl = vi.fn(() =>
      jsonResponse({
        output: 'ok',
        modelId: 'anthropic.claude-does-not-exist-v9:0',
        usage: { inputTokens: 1000, outputTokens: 1000 },
      }),
    );
    const be = new GatewayBackend({ gateway: 'http://gw', platform: 'p', fleet: 'f', fetchImpl });
    const r = await be.invoke(inv);
    expect(r.unpriced).toBe(true);
    expect(r.costUsd).toBe(0);
  });

  it('is unpriced when the gateway omits usage/modelId', async () => {
    const fetchImpl = vi.fn(() => jsonResponse({ content: 'text-only reply' }));
    const be = new GatewayBackend({ gateway: 'http://gw', platform: 'p', fleet: 'f', fetchImpl });
    const r = await be.invoke(inv);
    expect(r.output).toBe('text-only reply');
    expect(r.unpriced).toBe(true);
  });

  it('surfaces a guardrail intervention as a blocked result, not an error', async () => {
    const fetchImpl = vi.fn(() => jsonResponse({ output: '', stopReason: 'guardrail_intervened' }));
    const be = new GatewayBackend({ gateway: 'http://gw', platform: 'p', fleet: 'f', fetchImpl });
    const r = await be.invoke(inv);
    expect(r.guardrailBlocked).toBe(true);
    expect(r.stopReason).toBe('guardrail_intervened');
  });

  it('coerces an unknown stop reason to "other"', async () => {
    const fetchImpl = vi.fn(() => jsonResponse({ output: 'x', stopReason: 'weird_reason' }));
    const be = new GatewayBackend({ gateway: 'http://gw', platform: 'p', fleet: 'f', fetchImpl });
    const r = await be.invoke(inv);
    expect(r.stopReason).toBe('other');
  });

  it('throws a classified AgentError on a non-2xx gateway response', async () => {
    const fetchImpl = vi.fn(() => Promise.resolve(new Response('rate limited', { status: 429 })));
    const be = new GatewayBackend({ gateway: 'http://gw', platform: 'p', fleet: 'f', fetchImpl });
    await expect(be.invoke(inv)).rejects.toMatchObject({ class: 'RateLimit' });
  });

  it('wraps a transport failure as a Network AgentError', async () => {
    const fetchImpl = vi.fn(() => {
      throw new Error('ECONNREFUSED');
    });
    const be = new GatewayBackend({ gateway: 'http://gw', platform: 'p', fleet: 'f', fetchImpl });
    await expect(be.invoke(inv)).rejects.toMatchObject({ class: 'Network' });
  });
});

describe('classifyStatus', () => {
  it.each([
    [429, 'RateLimit'],
    [403, 'AuthFailure'],
    [401, 'AuthFailure'],
    [400, 'BadRequest'],
    [404, 'BadRequest'],
    [422, 'BadRequest'],
    [503, 'Overloaded'],
    [500, 'Server'],
  ] as const)('maps HTTP %i to %s', (status, cls) => {
    expect(classifyStatus(status)).toBe(cls);
  });
});

describe('asAgentError', () => {
  it('classifies an AbortError as Cancelled', () => {
    const err = Object.assign(new Error('aborted'), { name: 'AbortError' });
    expect(asAgentError(err, 'cid').class).toBe('Cancelled');
  });

  it('classifies a TimeoutError as Network', () => {
    const err = Object.assign(new Error('timed out'), { name: 'TimeoutError' });
    expect(asAgentError(err, 'cid').class).toBe('Network');
  });

  it('passes an existing AgentError through unchanged', () => {
    const original = new AgentError({ class: 'BadRequest', message: 'nope' });
    expect(asAgentError(original, 'cid')).toBe(original);
  });

  it('handles a non-Error throw', () => {
    expect(asAgentError('boom', 'cid').message).toBe('boom');
  });
});
