/* eslint-disable security/detect-non-literal-fs-filename --
   the suite reads/writes fixtures under an mkdtemp temp dir, not input paths. */
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { KNOWN_FLAGS, parseArgs, run, runEvaluate, runScore } from './cli.js';
import type { CaseResult, InvocationResult, ModelBackend } from './types.js';

let dir: string;
beforeEach(async () => {
  dir = await mkdtemp(join(tmpdir(), 'eval-runner-'));
});
afterEach(async () => {
  await rm(dir, { recursive: true, force: true });
  vi.unstubAllGlobals();
});

describe('parseArgs', () => {
  it('parses the space form and the = form', () => {
    const f = parseArgs(['--cases', '/tmp/c.json', '--gateway=http://gw', '--flag']);
    expect(f.get('--cases')).toBe('/tmp/c.json');
    expect(f.get('--gateway')).toBe('http://gw');
    expect(f.get('--flag')).toBe('');
  });
});

describe('KNOWN_FLAGS', () => {
  it('declares the evaluate + score flag sets', () => {
    expect(KNOWN_FLAGS.evaluate).toContain('--cases');
    expect(KNOWN_FLAGS.score).toContain('--score-out');
  });
});

const ok = (output: string): InvocationResult => ({
  output,
  latencyMs: 12,
  costUsd: 0.001,
  unpriced: false,
  guardrailBlocked: false,
});

describe('runEvaluate', () => {
  it('resolves cases, invokes the backend, and writes results.json', async () => {
    const casesPath = join(dir, 'cases.json');
    const outputPath = join(dir, 'results.json');
    await writeFile(
      casesPath,
      JSON.stringify([{ name: 'g', input: 'hi', expectContains: ['hello'] }]),
    );
    const backend: ModelBackend = { invoke: (i) => Promise.resolve(ok(`re:${i.input}`)) };
    await runEvaluate(
      { casesPath, platform: 'p', fleet: 'f', gateway: 'http://gw', outputPath },
      backend,
    );
    const results = JSON.parse(await readFile(outputPath, 'utf8')) as CaseResult[];
    expect(results[0]).toMatchObject({ name: 'g', output: 're:hi', expectContains: ['hello'] });
  });
});

describe('runScore', () => {
  it('grades results.json and writes score.json + report + junit', async () => {
    const resultsPath = join(dir, 'results.json');
    const reportPath = join(dir, 'report.html');
    const junitPath = join(dir, 'junit.xml');
    const scoreOutPath = join(dir, 'score.json');
    const results: CaseResult[] = [
      {
        name: 'g',
        input: 'hi',
        output: 'hello',
        latency_ms: 10,
        cost_usd: 0.001,
        unpriced: false,
        guardrailBlocked: false,
        expectContains: ['hello'],
      },
    ];
    await writeFile(resultsPath, JSON.stringify(results));
    await runScore({
      resultsPath,
      passThreshold: '0.85',
      reportPath,
      junitPath,
      scoreOutPath,
      now: () => '2026-07-17T00:00:00Z',
    });
    const score = JSON.parse(await readFile(scoreOutPath, 'utf8')) as {
      meanScore: string;
      passed: boolean;
    };
    expect(score).toMatchObject({ meanScore: '1.0000', passed: true });
    expect(await readFile(junitPath, 'utf8')).toContain('<testsuite');
    expect(await readFile(reportPath, 'utf8')).toContain('EvalSuite report');
  });
});

describe('run (dispatch)', () => {
  it('returns 2 for an unknown subcommand', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    expect(await run(['frobnicate'])).toBe(2);
  });

  it('returns 2 when a required flag is missing', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    expect(await run(['score', '--results', '/tmp/r.json'])).toBe(2);
  });

  it('returns 1 when the underlying command throws (missing file)', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const code = await run([
      'score',
      '--results',
      join(dir, 'does-not-exist.json'),
      '--pass-threshold',
      '0.85',
      '--report',
      join(dir, 'r.html'),
      '--junit',
      join(dir, 'j.xml'),
      '--score-out',
      join(dir, 's.json'),
    ]);
    expect(code).toBe(1);
  });

  it('dispatches evaluate end-to-end through the default gateway backend', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              output: 'hello world',
              stopReason: 'end_turn',
              modelId: 'anthropic.claude-sonnet-4-6',
              usage: { inputTokens: 10, outputTokens: 5 },
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
        ),
      ),
    );
    const casesPath = join(dir, 'cases.json');
    const outputPath = join(dir, 'results.json');
    await writeFile(
      casesPath,
      JSON.stringify([{ name: 'g', input: 'hi', expectContains: ['hello'] }]),
    );
    const code = await run([
      'evaluate',
      '--cases',
      casesPath,
      '--platform',
      'p',
      '--fleet',
      'f',
      '--gateway',
      'http://gw:8080',
      '--output',
      outputPath,
      '--timeout-ms',
      '5000',
    ]);
    expect(code).toBe(0);
    const results = JSON.parse(await readFile(outputPath, 'utf8')) as CaseResult[];
    expect(results[0]?.output).toBe('hello world');
    expect(results[0]?.unpriced).toBe(false);
  });
});
