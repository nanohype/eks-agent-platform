import Anthropic from '@anthropic-ai/sdk';

import { PROBES, type Probe, type ProbeResult } from './probes.js';

export interface RunOptions {
  /** The gateway endpoint, as published on `ModelGateway.status.endpoint`. */
  endpoint: string;
  /** A route name from `spec.routes[].name` — never a model id. */
  route: string;
  /** Per-call ceiling. A hung hop is a failure, not a hang. */
  timeoutMs?: number;
  probes?: Probe[];
}

export interface RunReport {
  baseURL: string;
  route: string;
  results: ProbeResult[];
  failed: number;
}

const DEFAULT_TIMEOUT_MS = 90_000;

/**
 * The base URL for a route whose wire format is Anthropic.
 *
 * Not the same value as `status.endpoint`: the gateway serves each wire format
 * under its own prefix, so the endpoint alone is not a usable base for any
 * client. This mirrors what a tenant app does, and it is deliberately the same
 * one-line derivation rather than an import — a conformance check that shared
 * the app's helper could not catch that helper being wrong.
 */
export function anthropicBaseURL(endpoint: string): string {
  return `${endpoint.replace(/\/+$/, '')}/anthropic`;
}

/**
 * Run every probe against one route, in sequence.
 *
 * Sequential on purpose. These share a prompt-cache prefix and a rate limit,
 * and running them concurrently makes the cache probe depend on which call
 * happened to land first — a flake that would read as a gateway fault.
 */
export async function runConformance(options: RunOptions): Promise<RunReport> {
  const baseURL = anthropicBaseURL(options.endpoint);
  const client = new Anthropic({
    baseURL,
    // The gateway holds the AWS credential and signs upstream; this app holds
    // none. The SDK requires the field and the gateway ignores it.
    apiKey: 'unused-the-gateway-holds-the-credential',
    timeout: options.timeoutMs ?? DEFAULT_TIMEOUT_MS,
    maxRetries: 0,
  });

  const results: ProbeResult[] = [];
  for (const probe of options.probes ?? PROBES) {
    try {
      const outcome = await probe.run(client, options.route);
      results.push({ name: probe.name, question: probe.question, ...outcome });
    } catch (err) {
      results.push({
        name: probe.name,
        question: probe.question,
        outcome: 'fail',
        detail: describeError(err),
      });
    }
  }

  return {
    baseURL,
    route: options.route,
    results,
    failed: results.filter((r) => r.outcome !== 'pass').length,
  };
}

/**
 * Render a thrown error into something that names the layer that refused.
 *
 * The status is what separates the failures: 404 is the route not matching
 * (the model header was never derived), 500 is the upstream refusing (usually
 * credentials), and a connect error is no data plane at all.
 */
export function describeError(err: unknown): string {
  const e = err as { status?: number; error?: unknown; message?: string };
  const body =
    typeof e?.error === 'object' && e.error !== null ? JSON.stringify(e.error) : (e?.message ?? '');
  const status = e?.status ? `HTTP ${e.status}` : 'no response';
  return `${status}: ${body}`.slice(0, 300);
}

/** Human-readable report. Returned rather than printed so callers choose. */
export function formatReport(report: RunReport): string {
  const rule = '─'.repeat(74);
  const lines = [
    rule,
    `gateway ${report.baseURL}   route "${report.route}"`,
    rule,
    ...report.results.flatMap((r) => [
      `${r.outcome === 'pass' ? '  ok  ' : '  XX  '} ${r.name.padEnd(20)} ${r.question}`,
      `         ${r.detail}`,
    ]),
    rule,
    report.failed === 0
      ? `all ${report.results.length} probes passed`
      : `${report.failed} of ${report.results.length} probes failed`,
  ];
  return lines.join('\n');
}
