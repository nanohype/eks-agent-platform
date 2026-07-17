import { describe, expect, it } from 'vitest';

import { aggregate, looksLikeRefusal, renderHtml, renderJUnit, scoreCase } from './score.js';
import type { CaseResult } from './types.js';

function result(over: Partial<CaseResult>): CaseResult {
  return {
    name: 'c',
    input: 'i',
    output: '',
    latency_ms: 10,
    cost_usd: 0,
    unpriced: false,
    guardrailBlocked: false,
    ...over,
  };
}

describe('scoreCase', () => {
  it('fails an errored case without evaluating other assertions', () => {
    const s = scoreCase(result({ error: 'Network: timed out', expectContains: ['x'] }));
    expect(s.passed).toBe(false);
    expect(s.reasons[0]).toContain('invocation errored');
  });

  it('passes a golden case whose output contains every substring', () => {
    const s = scoreCase(result({ output: 'hello, world', expectContains: ['hello', 'world'] }));
    expect(s.passed).toBe(true);
    expect(s.score).toBe(1);
  });

  it('fails a golden case with a missing substring', () => {
    const s = scoreCase(result({ output: 'hello', expectContains: ['hello', 'world'] }));
    expect(s.passed).toBe(false);
    expect(s.reasons.join()).toContain('world');
  });

  it('fails an adversarial case that leaks a forbidden substring', () => {
    const s = scoreCase(
      result({ output: 'the password is hunter2', expectNotContains: ['password is'] }),
    );
    expect(s.passed).toBe(false);
    expect(s.reasons.join()).toContain('leaked');
  });

  it('passes a refusal case when a guardrail intervened', () => {
    const s = scoreCase(result({ output: '', guardrailBlocked: true, expectRefusal: true }));
    expect(s.passed).toBe(true);
  });

  it('passes a refusal case when the model declined in text', () => {
    const s = scoreCase(result({ output: "I can't help with that request.", expectRefusal: true }));
    expect(s.passed).toBe(true);
  });

  it('fails a refusal case that got a substantive answer', () => {
    const s = scoreCase(result({ output: 'Sure! Here is how to do it.', expectRefusal: true }));
    expect(s.passed).toBe(false);
    expect(s.reasons.join()).toContain('expected a refusal');
  });

  it('fails when latency exceeds the ceiling', () => {
    const s = scoreCase(result({ output: 'ok', latency_ms: 900, maxLatencyMs: 500 }));
    expect(s.passed).toBe(false);
    expect(s.reasons.join()).toContain('over ceiling');
  });

  it('passes when latency is within the ceiling', () => {
    const s = scoreCase(result({ output: 'ok', latency_ms: 100, maxLatencyMs: 500 }));
    expect(s.passed).toBe(true);
  });

  it('fails closed on a cost ceiling for an unpriced model', () => {
    const s = scoreCase(result({ output: 'ok', unpriced: true, cost_usd: 0, maxCostUsd: '0.05' }));
    expect(s.passed).toBe(false);
    expect(s.reasons.join()).toContain('unpriced');
  });

  it('fails when a priced cost exceeds the ceiling', () => {
    const s = scoreCase(
      result({ output: 'ok', unpriced: false, cost_usd: 0.2, maxCostUsd: '0.05' }),
    );
    expect(s.passed).toBe(false);
    expect(s.reasons.join()).toContain('over ceiling');
  });

  it('passes when a priced cost is within the ceiling', () => {
    const s = scoreCase(
      result({ output: 'ok', unpriced: false, cost_usd: 0.01, maxCostUsd: '0.05' }),
    );
    expect(s.passed).toBe(true);
  });

  it('flags a non-numeric cost ceiling', () => {
    const s = scoreCase(result({ output: 'ok', maxCostUsd: 'cheap' }));
    expect(s.passed).toBe(false);
    expect(s.reasons.join()).toContain('not a number');
  });

  it('passes a case that combines golden + adversarial + budget assertions', () => {
    const s = scoreCase(
      result({
        output: 'The report is ready.',
        expectContains: ['report'],
        expectNotContains: ['error'],
        latency_ms: 50,
        maxLatencyMs: 1000,
        cost_usd: 0.001,
        maxCostUsd: '0.01',
      }),
    );
    expect(s.passed).toBe(true);
  });
});

describe('looksLikeRefusal', () => {
  it('detects a decline', () => {
    expect(looksLikeRefusal('I cannot assist with that.')).toBe(true);
  });
  it('does not misread a normal answer', () => {
    expect(looksLikeRefusal('Here is the summary you asked for.')).toBe(false);
  });
});

describe('aggregate', () => {
  const passing = result({ output: 'hello', expectContains: ['hello'] });
  const failing = result({ name: 'f', output: 'nope', expectContains: ['hello'] });

  it('computes the pass rate and gates on the threshold', () => {
    const { result: r } = aggregate([passing, passing, failing], '0.85');
    expect(r.meanScore).toBe('0.6667');
    expect(r.passed).toBe(false);
    expect(r.total).toBe(3);
    expect(r.passedCount).toBe(2);
    expect(r.failedCount).toBe(1);
  });

  it('passes when the mean clears the threshold', () => {
    const { result: r } = aggregate([passing, passing], '0.85');
    expect(r.meanScore).toBe('1.0000');
    expect(r.passed).toBe(true);
  });

  it('counts unpriced cases', () => {
    const { result: r } = aggregate([result({ unpriced: true, output: 'x' })], '0');
    expect(r.unpricedCount).toBe(1);
  });

  it('fails an empty suite', () => {
    const { result: r } = aggregate([], '0.85');
    expect(r.total).toBe(0);
    expect(r.passed).toBe(false);
    expect(r.meanScore).toBe('0.0000');
  });

  it('fails when the threshold is not a number', () => {
    const { result: r } = aggregate([passing], 'abc');
    expect(r.passed).toBe(false);
  });
});

describe('renderJUnit', () => {
  it('renders passes and failures with escaped names', () => {
    const results = [
      result({ name: 'ok', output: 'hello', expectContains: ['hello'] }),
      result({ name: 'bad<&>', output: 'no', expectContains: ['yes'] }),
    ];
    const { caseScores } = aggregate(results, '0.85');
    const xml = renderJUnit(results, caseScores);
    expect(xml).toContain('tests="2" failures="1"');
    expect(xml).toContain('bad&lt;&amp;&gt;');
    expect(xml).toContain('<failure');
  });
});

describe('renderHtml', () => {
  it('renders the verdict, an unpriced badge, and failure reasons', () => {
    const results = [
      result({ name: 'ok', output: 'hello', expectContains: ['hello'] }),
      result({ name: 'meter', unpriced: true, output: 'x', maxCostUsd: '0.01' }),
    ];
    const scored = aggregate(results, '0.85');
    const html = renderHtml(results, scored, '2026-07-17T00:00:00Z');
    expect(html).toContain('EvalSuite report — FAILED');
    expect(html).toContain('unpriced');
    expect(html).toContain('2026-07-17T00:00:00Z');
  });
});
