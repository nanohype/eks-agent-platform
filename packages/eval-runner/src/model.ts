import { AgentError, type ErrorClass } from '@eks-agent/core';
import { priceModel } from '@eks-agent/pricing';
import type { Message } from '@eks-agent/sdk';

import type { InvocationResult, ModelBackend, StopReason } from './types.js';

/**
 * The JSON shape the runner expects back from agentgateway's
 * `POST /v1/agents/<platform>-<fleet>/messages` endpoint. agentgateway proxies
 * to Bedrock and echoes token usage + the resolved model id, which is what lets
 * the runner price each call. Every field is optional so a thin gateway that
 * only returns text still yields a usable (unpriced) result rather than
 * throwing.
 */
export interface GatewayResponse {
  /** Model text. `output` is preferred; `content` is accepted as an alias. */
  output?: string;
  content?: string;
  stopReason?: string;
  /** Resolved Bedrock model id, used to price the call. */
  modelId?: string;
  usage?: {
    inputTokens?: number;
    outputTokens?: number;
    cacheReadTokens?: number;
    cacheWriteTokens?: number;
  };
}

export interface GatewayBackendOptions {
  /** Base URL of agentgateway, e.g. http://agentgateway.agentgateway.svc.cluster.local:8080 */
  gateway: string;
  /** Platform name — the first half of the agent route id. */
  platform: string;
  /** Fleet name — the second half of the agent route id. */
  fleet: string;
  /** Injectable fetch, defaulting to the global. Tests pass a stub. */
  fetchImpl?: typeof fetch;
}

const STOP_REASONS: ReadonlySet<StopReason> = new Set<StopReason>([
  'end_turn',
  'max_tokens',
  'stop_sequence',
  'tool_use',
  'guardrail_intervened',
  'other',
]);

function coerceStopReason(raw: string | undefined): StopReason | undefined {
  if (raw === undefined) return undefined;
  return STOP_REASONS.has(raw as StopReason) ? (raw as StopReason) : 'other';
}

/**
 * Map an HTTP status from the gateway onto the shared error taxonomy so
 * failures classify the same way a direct Bedrock call would.
 */
export function classifyStatus(status: number): ErrorClass {
  if (status === 429) return 'RateLimit';
  if (status === 403 || status === 401) return 'AuthFailure';
  if (status === 400 || status === 404 || status === 422) return 'BadRequest';
  if (status === 503) return 'Overloaded';
  return 'Server';
}

/**
 * Drive a case through agentgateway. Reuses the SDK's cost accounting
 * ({@link priceModel}, unpriced-aware) and error taxonomy ({@link AgentError})
 * rather than reimplementing them. The caller (runCases) owns the deadline and
 * threads its AbortSignal in; a gateway non-2xx becomes a classified
 * AgentError, a guardrail intervention becomes a normal (blocked) result.
 */
export class GatewayBackend implements ModelBackend {
  private readonly gateway: string;
  private readonly platform: string;
  private readonly fleet: string;
  private readonly fetchImpl: typeof fetch;

  constructor(opts: GatewayBackendOptions) {
    this.gateway = opts.gateway.replace(/\/+$/, '');
    this.platform = opts.platform;
    this.fleet = opts.fleet;
    this.fetchImpl = opts.fetchImpl ?? fetch;
  }

  private url(): string {
    return `${this.gateway}/v1/agents/${this.platform}-${this.fleet}/messages`;
  }

  async invoke(inv: {
    name: string;
    input: string;
    correlationId: string;
    signal: AbortSignal;
  }): Promise<InvocationResult> {
    const started = Date.now();
    const messages: Message[] = [{ role: 'user', content: inv.input }];
    let res: Response;
    try {
      res = await this.fetchImpl(this.url(), {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          'x-correlation-id': inv.correlationId,
        },
        body: JSON.stringify({ messages, correlationId: inv.correlationId }),
        signal: inv.signal,
      });
    } catch (err) {
      throw asAgentError(err, inv.correlationId);
    }

    if (!res.ok) {
      const detail = await safeText(res);
      throw new AgentError({
        class: classifyStatus(res.status),
        message: `gateway ${res.status} for ${inv.name}: ${detail}`,
        correlationId: inv.correlationId,
      });
    }

    const body = (await res.json()) as GatewayResponse;
    const output = body.output ?? body.content ?? '';
    const stopReason = coerceStopReason(body.stopReason);
    const guardrailBlocked = stopReason === 'guardrail_intervened';

    // Price the call when the gateway reported both a model id and usage.
    // Anything short of that is unpriced — surfaced, never silently $0.
    let costUsd = 0;
    let unpriced = true;
    let modelId: string | undefined;
    if (body.modelId !== undefined && body.usage !== undefined) {
      modelId = body.modelId;
      const priced = priceModel({
        modelId: body.modelId,
        tokens: {
          inputTokens: body.usage.inputTokens ?? 0,
          outputTokens: body.usage.outputTokens ?? 0,
          cacheReadTokens: body.usage.cacheReadTokens ?? 0,
          cacheWriteTokens: body.usage.cacheWriteTokens ?? 0,
        },
      });
      costUsd = priced.costUsd;
      unpriced = !priced.priced;
    }

    return {
      output,
      latencyMs: Date.now() - started,
      costUsd,
      unpriced,
      guardrailBlocked,
      ...(stopReason !== undefined ? { stopReason } : {}),
      ...(modelId !== undefined ? { modelId } : {}),
    };
  }
}

/** Classify a thrown fetch/abort error into the shared taxonomy. */
export function asAgentError(err: unknown, correlationId: string): AgentError {
  if (err instanceof AgentError) return err;
  const name = err && typeof err === 'object' && 'name' in err ? String(err.name) : '';
  let cls: ErrorClass = 'Network';
  if (name === 'AbortError') cls = 'Cancelled';
  else if (name === 'TimeoutError') cls = 'Network';
  return new AgentError({
    class: cls,
    message: err instanceof Error ? err.message : String(err),
    cause: err,
    correlationId,
  });
}

async function safeText(res: Response): Promise<string> {
  try {
    return (await res.text()).slice(0, 500);
  } catch {
    return '<unreadable body>';
  }
}
