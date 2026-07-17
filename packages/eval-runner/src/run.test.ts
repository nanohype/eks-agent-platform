import { AgentError } from '@eks-agent/core';
import { describe, expect, it, vi } from 'vitest';

import { runCases } from './run.js';
import type { EvalCase, InvocationResult, ModelBackend } from './types.js';

function backendOf(
  fn: (input: string) => InvocationResult | Promise<InvocationResult>,
): ModelBackend {
  return { invoke: (i) => Promise.resolve(fn(i.input)) };
}

const ok = (output: string): InvocationResult => ({
  output,
  latencyMs: 42,
  costUsd: 0.001,
  unpriced: false,
  guardrailBlocked: false,
  stopReason: 'end_turn',
});

describe('runCases', () => {
  it('invokes every case in order and echoes its assertion criteria', async () => {
    const cases: EvalCase[] = [
      { name: 'g', input: 'hi', expectContains: ['hello'], maxLatencyMs: 5000 },
      { name: 'a', input: 'leak', expectNotContains: ['secret'], expectRefusal: true },
    ];
    const results = await runCases(
      backendOf((input) => ok(`echo:${input}`)),
      cases,
    );
    expect(results.map((r) => r.name)).toEqual(['g', 'a']);
    expect(results[0]).toMatchObject({
      output: 'echo:hi',
      latency_ms: 42,
      cost_usd: 0.001,
      unpriced: false,
      expectContains: ['hello'],
      maxLatencyMs: 5000,
    });
    expect(results[1]).toMatchObject({ expectNotContains: ['secret'], expectRefusal: true });
  });

  it('records a thrown AgentError as an errored case without sinking the run', async () => {
    const cases: EvalCase[] = [
      { name: 'boom', input: 'x' },
      { name: 'fine', input: 'y' },
    ];
    const backend: ModelBackend = {
      invoke: (i) => {
        if (i.input === 'x') {
          return Promise.reject(new AgentError({ class: 'Network', message: 'timed out' }));
        }
        return Promise.resolve(ok('ok'));
      },
    };
    const results = await runCases(backend, cases);
    expect(results[0]?.error).toBe('Network: timed out');
    expect(results[0]?.output).toBe('');
    expect(results[0]?.unpriced).toBe(true);
    expect(results[1]?.output).toBe('ok');
  });

  it('classifies a non-AgentError throw through the shared taxonomy', async () => {
    const results = await runCases(
      backendOf(() => {
        throw new Error('kaboom');
      }),
      [{ name: 'c', input: 'z' }],
    );
    expect(results[0]?.error).toContain('Network');
    expect(results[0]?.error).toContain('kaboom');
  });

  it('uses an injected correlation-id factory', async () => {
    const seen: string[] = [];
    const backend: ModelBackend = {
      invoke: (i) => {
        seen.push(i.correlationId);
        return Promise.resolve(ok('x'));
      },
    };
    await runCases(backend, [{ name: 'c', input: 'z' }], {
      correlationId: (name) => `custom-${name}`,
    });
    expect(seen).toEqual(['custom-c']);
  });

  it('skips holes in the case array defensively', async () => {
    const spy = vi.fn(() => ok('x'));
    const cases = [{ name: 'c', input: 'z' }, undefined as unknown as EvalCase];
    const results = await runCases(backendOf(spy), cases);
    expect(results).toHaveLength(1);
    expect(spy).toHaveBeenCalledTimes(1);
  });
});
