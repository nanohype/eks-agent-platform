import type { MessagesResponse } from '@eks-agent/sdk';

/**
 * Stop reasons the runner understands, reused verbatim from the SDK's
 * ProviderAdapter contract so the eval path and the direct-invocation path
 * classify a guardrail intervention identically.
 */
export type StopReason = MessagesResponse['stopReason'];

/**
 * A single evaluation case, mirroring the `EvalCase` CRD type. The assertion
 * fields set on a case decide its kind — there is no separate discriminator:
 *
 * - Golden case: `expectContains` (and optionally `maxLatencyMs` / `maxCostUsd`).
 * - Adversarial / injection case: `expectNotContains` and/or `expectRefusal`.
 *
 * A case may combine both families; every assertion present must hold.
 */
export interface EvalCase {
  name: string;
  input: string;
  /** Golden assertion: output must contain every listed substring. */
  expectContains?: string[];
  /** Adversarial assertion: output must contain none of these substrings. */
  expectNotContains?: string[];
  /** Adversarial assertion: the agent must decline (guardrail block or refusal). */
  expectRefusal?: boolean;
  /** Latency ceiling in milliseconds; 0 / unset disables the check. */
  maxLatencyMs?: number;
  /** Per-call cost ceiling in USD, as a decimal string; unset disables the check. */
  maxCostUsd?: string;
}

/** What the model gateway returns for one case invocation. */
export interface InvocationResult {
  output: string;
  latencyMs: number;
  /** Estimated USD cost for the call (0 when unpriced — see `unpriced`). */
  costUsd: number;
  /**
   * True when the model id had no pricing entry, so `costUsd` is an unmetered 0
   * rather than a real $0. A cost assertion on an unpriced result fails closed.
   */
  unpriced: boolean;
  /** True when the gateway reported a guardrail intervention. */
  guardrailBlocked: boolean;
  stopReason?: StopReason;
  /** The resolved Bedrock model id the gateway routed to, when reported. */
  modelId?: string;
}

/** The transport the runner drives a case through. */
export interface ModelBackend {
  invoke(inv: {
    name: string;
    input: string;
    correlationId: string;
    signal: AbortSignal;
  }): Promise<InvocationResult>;
}

/**
 * One record in `results.json`, the artifact `evaluate` writes and `score`
 * reads. It is a superset of the WorkflowTemplate's documented
 * `{name, input, output, latency_ms, cost_usd, error?}` shape: the observation
 * fields keep those exact (snake_case) names, and the case's assertion fields
 * ride along so `score` can grade without re-reading the source suite — the
 * score step never receives `cases.json`.
 */
export interface CaseResult {
  name: string;
  input: string;
  output: string;
  latency_ms: number;
  cost_usd: number;
  unpriced: boolean;
  guardrailBlocked: boolean;
  stopReason?: StopReason;
  /** Present when the case could not be invoked (timeout, network, refusal-to-serve). */
  error?: string;
  // Echoed assertion criteria (from the source EvalCase).
  expectContains?: string[];
  expectNotContains?: string[];
  expectRefusal?: boolean;
  maxLatencyMs?: number;
  maxCostUsd?: string;
}

/** The grade for one case: pass/fail plus the reasons any assertion failed. */
export interface CaseScore {
  name: string;
  passed: boolean;
  /** 1 when every assertion held, 0 otherwise. */
  score: number;
  reasons: string[];
}

/**
 * `score.json` — what the `score` step emits and the WorkflowTemplate's
 * writeback reads. `meanScore` is a decimal string and `passed` a boolean, the
 * two fields the writeback's jq (`.meanScore`, `.passed`) depends on; the
 * report-url is injected by the workflow shell afterwards. The rest is
 * surfaced in the HTML report and is safe to extend.
 */
export interface ScoreResult {
  meanScore: string;
  passed: boolean;
  passThreshold: string;
  total: number;
  passedCount: number;
  failedCount: number;
  /** Number of cases whose cost was unmetered (unpriced model). */
  unpricedCount: number;
}
