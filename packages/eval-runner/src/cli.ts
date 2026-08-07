/* eslint-disable security/detect-non-literal-fs-filename --
   this CLI's contract is to read/write the file paths named by its flags
   (--cases/--output/--results/--report/--junit/--score-out); the paths are
   operator-supplied WorkflowTemplate parameters, not attacker input. */
import { readFile, writeFile } from 'node:fs/promises';

import { parseCases } from './cases.js';
import { GatewayBackend, type RouteAPI } from './model.js';
import { runCases } from './run.js';
import { aggregate, renderHtml, renderJUnit } from './score.js';
import type { CaseResult, ModelBackend } from './types.js';

/**
 * The flags each subcommand REQUIRES — every one is read with `required()`
 * below, so a template that stops passing one exits 2 on every run.
 *
 * Declared separately from KNOWN_FLAGS, and KNOWN_FLAGS is derived from it, so
 * the two cannot drift. `contract.test.ts` checks both directions against the
 * WorkflowTemplate: no flag the CLI does not accept, and no required flag the
 * template does not pass.
 */
export const REQUIRED_FLAGS: Record<'evaluate' | 'score', readonly string[]> = {
  evaluate: ['--cases', '--base-url', '--route', '--route-api', '--output'],
  score: ['--results', '--pass-threshold', '--report', '--junit', '--score-out'],
};

/** The flags each subcommand accepts: the required set plus the optional ones. */
export const KNOWN_FLAGS: Record<'evaluate' | 'score', readonly string[]> = {
  evaluate: [...REQUIRED_FLAGS.evaluate, '--timeout-ms'],
  score: [...REQUIRED_FLAGS.score],
};

export class UsageError extends Error {}

/** Parse `--flag value` / `--flag=value` argv into a flag map. */
export function parseArgs(argv: readonly string[]): Map<string, string> {
  const flags = new Map<string, string>();
  for (let i = 0; i < argv.length; i++) {
    const tok = argv.at(i);
    if (tok?.startsWith('--') !== true) continue;
    const eq = tok.indexOf('=');
    if (eq >= 0) {
      flags.set(tok.slice(0, eq), tok.slice(eq + 1));
      continue;
    }
    const next = argv.at(i + 1);
    if (next !== undefined && !next.startsWith('--')) {
      flags.set(tok, next);
      i++;
    } else {
      flags.set(tok, '');
    }
  }
  return flags;
}

function required(flags: Map<string, string>, name: string): string {
  const v = flags.get(name);
  if (v === undefined || v === '') throw new UsageError(`missing required flag ${name}`);
  return v;
}

export interface EvaluateOptions {
  casesPath: string;
  /** ModelGateway.status.routes[].baseURL — never status.endpoint. */
  baseURL: string;
  /** ModelGateway.status.routes[].name. */
  route: string;
  /** ModelGateway.status.routes[].api. */
  api: RouteAPI;
  outputPath: string;
  timeoutMs?: number;
}

/**
 * Parse the wire format the operator sent.
 *
 * Rejected rather than defaulted. The operator forwards the CRD's own value
 * (`Anthropic` / `OpenAI`); anything else means the two sides disagree about
 * the spelling, and falling back to a format would post one wire shape at the
 * other's endpoint and fail every case against a healthy-looking gateway.
 */
export function parseRouteAPI(raw: string): RouteAPI {
  if (raw === 'Anthropic' || raw === 'OpenAI') return raw;
  throw new UsageError(
    `--route-api ${JSON.stringify(raw)} is neither Anthropic nor OpenAI; the operator sends ` +
      'ModelGateway.status.routes[].api verbatim, so this means the two sides disagree about the spelling',
  );
}

/**
 * `evaluate` — resolve the case list, invoke each against the platform's model
 * gateway, and write results.json. Backend is injectable for tests; production
 * uses the {@link GatewayBackend}.
 */
export async function runEvaluate(opts: EvaluateOptions, backend?: ModelBackend): Promise<void> {
  const cases = parseCases(await readFile(opts.casesPath, 'utf8'));
  const b =
    backend ?? new GatewayBackend({ baseURL: opts.baseURL, route: opts.route, api: opts.api });
  const results = await runCases(b, cases, {
    ...(opts.timeoutMs !== undefined ? { timeoutMs: opts.timeoutMs } : {}),
  });
  await writeFile(opts.outputPath, JSON.stringify(results));
}

export interface ScoreOptions {
  resultsPath: string;
  passThreshold: string;
  reportPath: string;
  junitPath: string;
  scoreOutPath: string;
  /** Injectable clock for deterministic reports in tests. */
  now?: () => string;
}

/**
 * `score` — grade results.json against the pass threshold, render the HTML +
 * JUnit reports, and write score.json (`{meanScore, passed, …}`, the shape the
 * workflow's writeback step patches into EvalSuite.status).
 */
export async function runScore(opts: ScoreOptions): Promise<void> {
  const results = JSON.parse(await readFile(opts.resultsPath, 'utf8')) as CaseResult[];
  const scored = aggregate(results, opts.passThreshold);
  const generatedAt = (opts.now ?? (() => new Date().toISOString()))();
  await writeFile(opts.reportPath, renderHtml(results, scored, generatedAt));
  await writeFile(opts.junitPath, renderJUnit(results, scored.caseScores));
  await writeFile(opts.scoreOutPath, JSON.stringify(scored.result));
}

/** Dispatch a full argv (subcommand + flags). Returns a process exit code. */
export async function run(argv: readonly string[]): Promise<number> {
  const sub = argv[0];
  const flags = parseArgs(argv.slice(1));
  try {
    if (sub === 'evaluate') {
      await runEvaluate({
        casesPath: required(flags, '--cases'),
        baseURL: required(flags, '--base-url'),
        route: required(flags, '--route'),
        api: parseRouteAPI(required(flags, '--route-api')),
        outputPath: required(flags, '--output'),
        ...(flags.has('--timeout-ms')
          ? { timeoutMs: Number.parseInt(required(flags, '--timeout-ms'), 10) }
          : {}),
      });
      return 0;
    }
    if (sub === 'score') {
      await runScore({
        resultsPath: required(flags, '--results'),
        passThreshold: required(flags, '--pass-threshold'),
        reportPath: required(flags, '--report'),
        junitPath: required(flags, '--junit'),
        scoreOutPath: required(flags, '--score-out'),
      });
      return 0;
    }
    console.error(`unknown subcommand: ${sub ?? '<none>'} (expected 'evaluate' or 'score')`);
    return 2;
  } catch (err) {
    if (err instanceof UsageError) {
      console.error(`usage error: ${err.message}`);
      return 2;
    }
    console.error(err instanceof Error ? (err.stack ?? err.message) : String(err));
    return 1;
  }
}
