import { AgentError, type CallEvent, type ModelFamily } from '@eks-agent/core';

import { AnthropicBedrockAdapter } from './adapters/anthropic.js';
import { type BedrockAdapter, type BedrockAdapterOptions } from './adapters/bedrock-base.js';
import { NovaBedrockAdapter } from './adapters/nova.js';
import type { MessagesParams, MessagesResponse } from './types.js';

type AdapterCtor = new (opts: BedrockAdapterOptions) => BedrockAdapter;

const REGISTRY: Partial<Record<ModelFamily, AdapterCtor>> = {
  anthropic: AnthropicBedrockAdapter,
  'amazon-nova': NovaBedrockAdapter,
};

/**
 * Construct the BedrockAdapter for a given model family. Throws if the
 * family has no shipped adapter — the alternative (silent fallback to a
 * default) would silently mis-route invocations.
 *
 * Adding a new family is a registry insert here plus a new subclass; ADR
 * 0002 names this contract explicitly.
 */
export function createBedrockAdapter(
  family: ModelFamily,
  opts: BedrockAdapterOptions,
): BedrockAdapter {
  // eslint-disable-next-line security/detect-object-injection
  const ctor = REGISTRY[family];
  if (!ctor) {
    throw new Error(
      `no BedrockAdapter registered for model family '${family}'. Shipped: ${Object.keys(REGISTRY).join(', ')}. To support another family, subclass BedrockAdapter and register the constructor in REGISTRY (packages/sdk/src/factory.ts).`,
    );
  }
  return new ctor(opts);
}

/** Families with a shipped BedrockAdapter implementation. */
export function shippedFamilies(): ModelFamily[] {
  return Object.keys(REGISTRY) as ModelFamily[];
}

// ───────────────────────────── model router ─────────────────────────────

/** One rung of a fallback chain: which family adapter, which model id. */
export interface RouteTarget {
  modelFamily: ModelFamily;
  modelId: string;
}

/** A single failed attempt captured while walking the chain. */
export interface RouteAttempt {
  target: RouteTarget;
  error: AgentError;
}

/**
 * Thrown when every target in the chain failed. Carries the per-target errors
 * in attempt order so the caller can see exactly what each rung returned.
 */
export class ChainExhaustedError extends Error {
  readonly attempts: readonly RouteAttempt[];

  constructor(attempts: readonly RouteAttempt[]) {
    const summary = attempts.map((a) => `${a.target.modelId} (${a.error.class})`).join(' → ');
    super(`model router chain exhausted after ${attempts.length} attempt(s): ${summary}`);
    this.name = 'ChainExhaustedError';
    this.attempts = attempts;
  }
}

export interface ModelRouterOptions extends BedrockAdapterOptions {
  /**
   * Sink for per-attempt call events. A successful attempt emits an `ok`
   * CallEvent (from the adapter); every failed attempt emits an `error`
   * CallEvent from the router — so cost/usage accounting sees the failed rungs
   * too, not only the one that ultimately answered.
   */
  onCallEvent?: (event: CallEvent) => void;
  /**
   * Adapter constructor override. Defaults to {@link createBedrockAdapter};
   * primarily a test seam for injecting fakes, and an extension point for
   * custom adapters.
   */
  adapterFor?: (family: ModelFamily, opts: BedrockAdapterOptions) => BedrockAdapter;
}

/** Call params for the router — modelId/modelFamily come from the chain. */
export type RouterMessagesParams = Omit<MessagesParams, 'modelId' | 'modelFamily'>;

// Error classes the router never retries on a different rung: a caller
// cancellation, a hard budget stop, and a guardrail intervention are deliberate
// terminal outcomes — spending the fallback on them would waste a call or, for
// the guardrail, attempt to route around a policy decision.
const TERMINAL: ReadonlySet<AgentError['class']> = new Set<AgentError['class']>([
  'Cancelled',
  'BudgetExceeded',
  'GuardrailBlock',
]);

const zeroTokens = { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0 };

/**
 * Ordered `[primary, fallback, …]` adapter chain. `messages` tries each rung in
 * turn; on a non-terminal failure it emits an error CallEvent for that rung and
 * advances to the next model. It resolves with the first rung that answers, or
 * throws {@link ChainExhaustedError} when the whole chain fails.
 */
export class ModelRouter {
  private readonly adapters = new Map<ModelFamily, BedrockAdapter>();

  constructor(
    private readonly chain: readonly RouteTarget[],
    private readonly opts: ModelRouterOptions,
  ) {
    if (chain.length === 0) {
      throw new Error('createModelRouter requires at least one RouteTarget in the chain');
    }
    const build = opts.adapterFor ?? createBedrockAdapter;
    for (const target of chain) {
      if (this.adapters.has(target.modelFamily)) continue;
      const adapter = build(target.modelFamily, opts);
      // Success events flow through the adapter's own telemetry hook.
      if (opts.onCallEvent) adapter.emitCallEvent = opts.onCallEvent;
      this.adapters.set(target.modelFamily, adapter);
    }
  }

  async messages(params: RouterMessagesParams): Promise<MessagesResponse> {
    const attempts: RouteAttempt[] = [];
    for (const target of this.chain) {
      const adapter = this.adapters.get(target.modelFamily);
      // The constructor built an adapter for every family in the chain, so this
      // is always set; the guard keeps the type honest.
      if (!adapter) continue;
      const started = Date.now();
      try {
        return await adapter.messages({
          ...params,
          modelId: target.modelId,
          modelFamily: target.modelFamily,
        });
      } catch (err) {
        const ae =
          err instanceof AgentError
            ? err
            : new AgentError({
                class: 'Server',
                message: err instanceof Error ? err.message : String(err),
                cause: err,
                correlationId: params.correlationId,
              });
        if (TERMINAL.has(ae.class)) throw ae;
        this.emitError(params, target, ae, Date.now() - started);
        attempts.push({ target, error: ae });
      }
    }
    throw new ChainExhaustedError(attempts);
  }

  private emitError(
    params: RouterMessagesParams,
    target: RouteTarget,
    err: AgentError,
    latencyMs: number,
  ): void {
    if (!this.opts.onCallEvent) return;
    this.opts.onCallEvent({
      correlationId: params.correlationId,
      platform: this.opts.platform ?? '',
      tenant: this.opts.tenant ?? '',
      modelFamily: target.modelFamily,
      modelId: target.modelId,
      tokens: { ...zeroTokens },
      // A failed InvokeModel bills nothing; the event exists so the attempt is
      // visible to accounting, with its errorClass, not to add spend.
      costUsd: 0,
      latencyMs,
      status: 'error',
      errorClass: err.class,
      timestamp: new Date().toISOString(),
      ...(this.opts.workspace !== undefined ? { workspace: this.opts.workspace } : {}),
    });
  }
}

/**
 * Build a fallback {@link ModelRouter} over an ordered chain of targets. The
 * first entry is the primary; each subsequent entry is tried in order when the
 * previous rung fails with a non-terminal error (throttling, overload, server,
 * network, or a model unavailable in-region).
 */
export function createModelRouter(chain: RouteTarget[], opts: ModelRouterOptions): ModelRouter {
  return new ModelRouter(chain, opts);
}
