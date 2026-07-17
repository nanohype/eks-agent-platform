import { AgentError } from '@eks-agent/core';

import { asAgentError } from './model.js';
import type { CaseResult, EvalCase, ModelBackend } from './types.js';

export interface RunOptions {
  /** Per-case deadline in ms. A case that overruns is recorded as errored. */
  timeoutMs?: number;
  /**
   * Correlation-id factory (defaults to a suite-scoped counter). Injectable so
   * tests get stable ids.
   */
  correlationId?: (caseName: string, index: number) => string;
}

const DEFAULT_TIMEOUT_MS = 60_000;

/**
 * Execute every case against the backend, in declaration order, and return one
 * {@link CaseResult} each. Each case runs under its own deadline (the SDK's
 * safety-net timeout idiom: `AbortSignal.timeout`); a case that times out,
 * is refused service, or otherwise throws is recorded with an `error` and an
 * empty output rather than aborting the whole run — one broken case must not
 * sink the suite. The case's assertion criteria are echoed into the result so
 * the scoring step can grade without the source suite.
 */
export async function runCases(
  backend: ModelBackend,
  cases: EvalCase[],
  opts: RunOptions = {},
): Promise<CaseResult[]> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const mkId = opts.correlationId ?? ((name, i) => `eval-${String(i)}-${name}`);
  const results: CaseResult[] = [];

  for (let i = 0; i < cases.length; i++) {
    const c = cases.at(i);
    if (c === undefined) continue;
    const correlationId = mkId(c.name, i);
    const signal = AbortSignal.timeout(timeoutMs);
    const base = echoCriteria(c);

    try {
      const inv = await backend.invoke({
        name: c.name,
        input: c.input,
        correlationId,
        signal,
      });
      results.push({
        ...base,
        output: inv.output,
        latency_ms: inv.latencyMs,
        cost_usd: inv.costUsd,
        unpriced: inv.unpriced,
        guardrailBlocked: inv.guardrailBlocked,
        ...(inv.stopReason !== undefined ? { stopReason: inv.stopReason } : {}),
      });
    } catch (err) {
      const agentErr = err instanceof AgentError ? err : asAgentError(err, correlationId);
      results.push({
        ...base,
        output: '',
        latency_ms: 0,
        cost_usd: 0,
        unpriced: true,
        guardrailBlocked: false,
        error: `${agentErr.class}: ${agentErr.message}`,
      });
    }
  }

  return results;
}

/** Seed a result with the case identity + its echoed assertion criteria. */
function echoCriteria(
  c: EvalCase,
): Pick<
  CaseResult,
  | 'name'
  | 'input'
  | 'expectContains'
  | 'expectNotContains'
  | 'expectRefusal'
  | 'maxLatencyMs'
  | 'maxCostUsd'
> {
  const base: ReturnType<typeof echoCriteria> = { name: c.name, input: c.input };
  if (c.expectContains !== undefined) base.expectContains = c.expectContains;
  if (c.expectNotContains !== undefined) base.expectNotContains = c.expectNotContains;
  if (c.expectRefusal !== undefined) base.expectRefusal = c.expectRefusal;
  if (c.maxLatencyMs !== undefined) base.maxLatencyMs = c.maxLatencyMs;
  if (c.maxCostUsd !== undefined) base.maxCostUsd = c.maxCostUsd;
  return base;
}
