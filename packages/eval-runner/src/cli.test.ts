/* eslint-disable security/detect-non-literal-fs-filename --
   the suite reads/writes fixtures under an mkdtemp temp dir, not input paths. */
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { KNOWN_FLAGS, parseArgs, parseRouteAPI, run, runEvaluate, runScore } from './cli.js';
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
      { casesPath, baseURL: 'http://gw/anthropic', route: 'chat', api: 'Anthropic', outputPath },
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
              model: 'anthropic.claude-sonnet-4-6',
              stop_reason: 'end_turn',
              content: [{ type: 'text', text: 'hello world' }],
              usage: { input_tokens: 10, output_tokens: 5 },
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
      '--base-url',
      'http://gw:8080/anthropic',
      '--route',
      'chat',
      '--route-api',
      'Anthropic',
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

describe('parseRouteAPI', () => {
  // The operator sends ModelGateway.status.routes[].api verbatim, and the CRD
  // enum is capitalised (Anthropic;OpenAI). A lowercase union on this side would
  // typecheck, run, and pick the wrong request body and response parser against a
  // gateway that reports healthy — the exact failure this whole change closes,
  // reintroduced one layer down. So the rejection is asserted, not assumed.
  it('accepts exactly the two spellings the CRD enum declares', () => {
    expect(parseRouteAPI('Anthropic')).toBe('Anthropic');
    expect(parseRouteAPI('OpenAI')).toBe('OpenAI');
  });

  it.each(['anthropic', 'openai', 'OPENAI', 'Bedrock', ''])(
    'refuses %o rather than guessing a wire format',
    (raw) => {
      expect(() => parseRouteAPI(raw)).toThrow(/neither Anthropic nor OpenAI/);
    },
  );
});

describe('run', () => {
  it('refuses an unknown subcommand instead of defaulting to one', async () => {
    const err = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    // 2, not 1 — this CLI reserves 2 for a usage error so a caller can tell
    // 'you invoked me wrong' from 'the run failed'.
    await expect(run(['wat'])).resolves.toBe(2);
    expect(err).toHaveBeenCalledWith(expect.stringContaining('unknown subcommand: wat'));
    err.mockRestore();
  });
});
