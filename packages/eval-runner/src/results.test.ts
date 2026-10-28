import { describe, expect, it } from 'vitest';
import { ZodError } from 'zod';

import { parseResults } from './results.js';
import { runCases } from './run.js';
import type { CaseResult, ModelBackend } from './types.js';

const record = {
  name: 'g1',
  input: 'i',
  output: 'hello world',
  latency_ms: 10,
  cost_usd: 0.001,
  unpriced: false,
  guardrailBlocked: false,
  expectContains: ['hello'],
};

describe('parseResults', () => {
  it('accepts a record carrying every CaseResult field', () => {
    // Required<> makes a field added to CaseResult a compile error here until
    // this record carries it, and the schema is strict, so a field the schema
    // does not also declare then fails this parse.
    const full: Required<CaseResult> = {
      ...record,
      stopReason: 'end_turn',
      error: '',
      expectNotContains: ['secret'],
      expectRefusal: false,
      maxLatencyMs: 5000,
      maxCostUsd: '0.01',
    };
    expect(parseResults(JSON.stringify([full]))).toEqual([full]);
  });

  it('round-trips what evaluate writes, including an errored case', async () => {
    const backend: ModelBackend = {
      invoke: ({ name }) =>
        name === 'down'
          ? Promise.reject(new Error('connect ECONNREFUSED'))
          : Promise.resolve({
              output: 'hello',
              latencyMs: 3,
              costUsd: 0.002,
              unpriced: false,
              guardrailBlocked: false,
              stopReason: 'end_turn',
            }),
    };
    const written = await runCases(backend, [
      { name: 'up', input: 'hi', expectContains: ['hello'], maxCostUsd: '0.01' },
      { name: 'down', input: 'hi', expectRefusal: true },
    ]);
    expect(parseResults(JSON.stringify(written))).toEqual(written);
  });

  it.each([
    ['a missing field', { ...record, unpriced: undefined }, 'unpriced'],
    ['a field of the wrong type', { ...record, latency_ms: '10' }, 'latency_ms'],
    ['a key evaluate does not write', { ...record, latencyMs: 10 }, 'latencyMs'],
    ['a stop reason outside the union', { ...record, stopReason: 'refused' }, 'stopReason'],
  ])('rejects %s, naming it', (_what, bad, field) => {
    const parse = () => parseResults(JSON.stringify([bad]));
    expect(parse).toThrow(ZodError);
    expect(parse).toThrow(field);
  });

  it('rejects a document that is not an array of records', () => {
    expect(() => parseResults(JSON.stringify(record))).toThrow(ZodError);
  });
});
