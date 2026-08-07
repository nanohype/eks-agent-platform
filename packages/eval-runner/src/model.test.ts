import { AgentError } from '@eks-agent/core';
import { describe, expect, it, vi } from 'vitest';

import { asAgentError, classifyStatus, GatewayBackend } from './model.js';

function jsonResponse(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'content-type': 'application/json' },
    }),
  );
}

const inv = { name: 'c', input: 'hi', correlationId: 'cid', signal: AbortSignal.timeout(5000) };

/** One text block, the shape the Anthropic Messages API actually returns. */
function textBlocks(text: string) {
  return [{ type: 'text', text }];
}

describe('GatewayBackend — Anthropic route', () => {
  it('posts to the published base + /v1/messages with the ROUTE NAME as model', async () => {
    const fetchImpl = vi.fn<typeof fetch>(() =>
      jsonResponse({
        model: 'anthropic.claude-sonnet-4-6',
        stop_reason: 'end_turn',
        content: textBlocks('hello there'),
        usage: { input_tokens: 1000, output_tokens: 500 },
      }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw:8080/anthropic/',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
    const r = await be.invoke(inv);

    // Trailing slash normalized away; the path is the one an Anthropic SDK
    // appends to the published base, not an agent path this runner invented.
    expect(fetchImpl.mock.calls[0]?.[0]).toBe('http://gw:8080/anthropic/v1/messages');

    // The gateway derives x-ai-eg-model from the body. A body with no `model`
    // matches no route rule, and a Bedrock model id there matches none either —
    // modelNameOverride does that substitution upstream.
    const sent = JSON.parse(String(fetchImpl.mock.calls[0]?.[1]?.body)) as {
      model?: string;
      max_tokens?: number;
      messages?: { role: string; content: string }[];
    };
    expect(sent.model).toBe('chat');
    expect(sent.max_tokens).toBeGreaterThan(0);
    expect(sent.messages).toEqual([{ role: 'user', content: 'hi' }]);

    // content is a block array; joining it is what keeps `output` a string.
    expect(r.output).toBe('hello there');
    expect(r.unpriced).toBe(false);
    // (1000/1e6)*3 + (500/1e6)*15 = 0.0105
    expect(r.costUsd).toBeCloseTo(0.0105, 8);
    expect(r.guardrailBlocked).toBe(false);
    expect(r.stopReason).toBe('end_turn');
  });

  it('strips a cross-region prefix before pricing', async () => {
    const fetchImpl = vi.fn(() =>
      jsonResponse({
        model: 'us.anthropic.claude-sonnet-4-6',
        content: textBlocks('ok'),
        usage: { input_tokens: 1000, output_tokens: 0 },
      }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw/anthropic',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(r.unpriced).toBe(false);
    expect(r.costUsd).toBeCloseTo(0.003, 8);
  });

  it('marks an unpriced model without pretending $0 is free', async () => {
    const fetchImpl = vi.fn(() =>
      jsonResponse({
        model: 'anthropic.claude-does-not-exist-v9:0',
        content: textBlocks('ok'),
        usage: { input_tokens: 1000, output_tokens: 1000 },
      }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw/anthropic',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(r.unpriced).toBe(true);
    expect(r.costUsd).toBe(0);
  });

  it('is unpriced when the response omits usage', async () => {
    const fetchImpl = vi.fn(() => jsonResponse({ content: textBlocks('text-only reply') }));
    const be = new GatewayBackend({
      baseURL: 'http://gw/anthropic',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(r.output).toBe('text-only reply');
    expect(r.unpriced).toBe(true);
  });

  it('surfaces a Bedrock guardrail intervention as a blocked result, not an error', async () => {
    // The surface a guardrail actually uses. Nothing in this repo produced
    // `guardrail_intervened` before, so every expectRefusal case was graded on
    // refusal-phrase matching alone.
    const fetchImpl = vi.fn(() =>
      jsonResponse({
        model: 'anthropic.claude-sonnet-4-6',
        stop_reason: 'end_turn',
        content: textBlocks(''),
        'amazon-bedrock-guardrailAction': 'INTERVENED',
      }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw/anthropic',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(r.guardrailBlocked).toBe(true);
    expect(r.stopReason).toBe('guardrail_intervened');
  });

  it('coerces an unknown stop reason to "other"', async () => {
    const fetchImpl = vi.fn(() =>
      jsonResponse({ content: textBlocks('x'), stop_reason: 'weird_reason' }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw/anthropic',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(r.stopReason).toBe('other');
  });
});

describe('GatewayBackend — OpenAI route', () => {
  it('posts to the published base + /chat/completions and reads choices[0]', async () => {
    const fetchImpl = vi.fn<typeof fetch>(() =>
      jsonResponse({
        model: 'anthropic.claude-sonnet-4-6',
        choices: [{ message: { content: 'hello there' }, finish_reason: 'stop' }],
        usage: { prompt_tokens: 1000, completion_tokens: 500 },
      }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw:8080/v1',
      route: 'chat',
      api: 'OpenAI',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(fetchImpl.mock.calls[0]?.[0]).toBe('http://gw:8080/v1/chat/completions');
    expect(r.output).toBe('hello there');
    expect(r.stopReason).toBe('end_turn');
    expect(r.costUsd).toBeCloseTo(0.0105, 8);
  });

  it('does not double-charge cached prompt tokens', async () => {
    // OpenAI's prompt_tokens is the TOTAL and already includes cached_tokens.
    // priceModel sums input + cacheRead, so passing both whole bills the cached
    // half twice — and maxCostUsd grades against that number.
    const fetchImpl = vi.fn(() =>
      jsonResponse({
        model: 'anthropic.claude-sonnet-4-6',
        choices: [{ message: { content: 'ok' }, finish_reason: 'stop' }],
        usage: {
          prompt_tokens: 1000,
          completion_tokens: 0,
          prompt_tokens_details: { cached_tokens: 800 },
        },
      }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw/v1',
      route: 'chat',
      api: 'OpenAI',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    // 200 fresh @ $3/M + 800 cached @ $0.30/M = 0.0006 + 0.00024
    expect(r.costUsd).toBeCloseTo(0.00084, 8);
  });

  it('maps content_filter onto a guardrail block', async () => {
    const fetchImpl = vi.fn(() =>
      jsonResponse({
        choices: [{ message: { content: '' }, finish_reason: 'content_filter' }],
      }),
    );
    const be = new GatewayBackend({
      baseURL: 'http://gw/v1',
      route: 'chat',
      api: 'OpenAI',
      fetchImpl,
    });
    const r = await be.invoke(inv);
    expect(r.guardrailBlocked).toBe(true);
    expect(r.stopReason).toBe('guardrail_intervened');
  });
});

describe('GatewayBackend — transport', () => {
  it('throws a classified AgentError on a non-2xx gateway response', async () => {
    const fetchImpl = vi.fn(() => Promise.resolve(new Response('rate limited', { status: 429 })));
    const be = new GatewayBackend({
      baseURL: 'http://gw/anthropic',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
    await expect(be.invoke(inv)).rejects.toMatchObject({ class: 'RateLimit' });
  });

  it('wraps a transport failure as a Network AgentError', async () => {
    const fetchImpl = vi.fn(() => {
      throw new Error('ECONNREFUSED');
    });
    const be = new GatewayBackend({
      baseURL: 'http://gw/anthropic',
      route: 'chat',
      api: 'Anthropic',
      fetchImpl,
    });
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
