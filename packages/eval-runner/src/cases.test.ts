import { describe, expect, it } from 'vitest';

import { parseCases } from './cases.js';

describe('parseCases', () => {
  it('parses a golden case and keeps its positive assertion', () => {
    const cases = parseCases(
      JSON.stringify([
        {
          name: 'g',
          input: 'hi',
          expectContains: ['hello'],
          maxLatencyMs: 5000,
          maxCostUsd: '0.01',
        },
      ]),
    );
    expect(cases).toEqual([
      { name: 'g', input: 'hi', expectContains: ['hello'], maxLatencyMs: 5000, maxCostUsd: '0.01' },
    ]);
  });

  it('parses an adversarial case (notContains + refusal)', () => {
    const cases = parseCases(
      JSON.stringify([
        { name: 'a', input: 'leak', expectNotContains: ['secret'], expectRefusal: true },
      ]),
    );
    expect(cases).toEqual([
      { name: 'a', input: 'leak', expectNotContains: ['secret'], expectRefusal: true },
    ]);
  });

  it("treats the operator's null / empty defaults as absent assertions", () => {
    // Exactly what buildInlineCasesParam emits for a case with no forbidden
    // list, no refusal, no ceilings.
    const cases = parseCases(
      JSON.stringify([
        {
          name: 'g',
          input: 'hi',
          expectContains: ['hello'],
          expectNotContains: null,
          expectRefusal: false,
          maxLatencyMs: 0,
          maxCostUsd: '',
        },
      ]),
    );
    expect(cases).toEqual([{ name: 'g', input: 'hi', expectContains: ['hello'] }]);
  });

  it('rejects an unknown field (strict schema)', () => {
    expect(() => parseCases(JSON.stringify([{ name: 'x', input: 'y', bogus: 1 }]))).toThrow();
  });

  it('rejects a case missing a name', () => {
    expect(() => parseCases(JSON.stringify([{ input: 'y' }]))).toThrow();
  });

  it('rejects non-array input', () => {
    expect(() => parseCases(JSON.stringify({ name: 'x', input: 'y' }))).toThrow();
  });
});
