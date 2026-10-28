import Anthropic, { type ClientOptions } from '@anthropic-ai/sdk';

import { PROBES, type Probe, type ProbeResult } from './probes.js';

/**
 * The client's one credential. The gateway holds the AWS credential and signs
 * upstream, so it has no use for any key this client could send.
 */
export const PLACEHOLDER_API_KEY = 'unused-the-gateway-holds-the-credential';

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
  // Trimmed by scanning rather than with /\/+$/. That pattern backtracks
  // polynomially on a string of many slashes, and this value arrives from
  // outside — an operator-published status field or an environment variable.
  let end = endpoint.length;
  while (end > 0 && endpoint[end - 1] === '/') end -= 1;
  return `${endpoint.slice(0, end)}/anthropic`;
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
  // The client sends its SDK's default `anthropic-version`. Bedrock reads the
  // version from the request body, and the gateway writes the
  // AIServiceBackend's schema version there on every request it forwards,
  // whatever the client sent. The operator sets that schema version from its
  // `bedrockAnthropicVersion` constant
  // (operators/internal/controller/modelgateway_reconcile.go). A version set
  // here would change nothing upstream, and would stop the suite sending what
  // a tenant on the same SDK sends.
  const client = clientWithoutEnvironmentHeaders({
    baseURL,
    // Whatever the environment or an on-disk profile holds, the only
    // credential this client sends is the placeholder, as
    // `Authorization: Bearer`, with no `X-Api-Key`. Passing it here stops the
    // SDK resolving ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, a profile or
    // workload identity; clientWithoutEnvironmentHeaders keeps
    // ANTHROPIC_CUSTOM_HEADERS from replacing it. Envoy AI Gateway's AWS
    // backend auth, which the operator's AWSCredentials BackendSecurityPolicy
    // selects, signs each upstream request with SigV4 and sets that request's
    // Authorization header in place of the placeholder.
    apiKey: PLACEHOLDER_API_KEY,
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
 * Construct the client with ANTHROPIC_CUSTOM_HEADERS out of its sight.
 *
 * The SDK reads that variable once, in its constructor, and applies the
 * headers it lists after the credential, so an `Authorization` or `X-Api-Key`
 * line there replaces the placeholder. Pinning those two in `defaultHeaders`
 * does not hold: the SDK spreads `defaultHeaders` over the parsed variable by
 * exact key, then applies the result case-insensitively in key order, so a
 * second, differently-cased line in the variable lands after the pinned one
 * and wins. Filtering the variable down to its non-credential headers does not
 * hold either, because a credential can travel under any header name. So the
 * variable is hidden for the synchronous constructor call and none of it is
 * sent.
 */
function clientWithoutEnvironmentHeaders(options: ClientOptions): Anthropic {
  const customHeaders = process.env.ANTHROPIC_CUSTOM_HEADERS;
  delete process.env.ANTHROPIC_CUSTOM_HEADERS;
  try {
    return new Anthropic(options);
  } finally {
    if (customHeaders !== undefined) process.env.ANTHROPIC_CUSTOM_HEADERS = customHeaders;
  }
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
