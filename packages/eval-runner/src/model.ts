import { AgentError, type ErrorClass } from '@eks-agent/core';
import { priceModel } from '@eks-agent/pricing';

import type { InvocationResult, ModelBackend, StopReason } from './types.js';

/**
 * The wire format the route is served under, spelled exactly as the CRD spells
 * it — `RouteAPI` in operators/api/agents/v1alpha1/modelgateway_types.go, whose
 * kubebuilder Enum is `Anthropic;OpenAI`.
 *
 * The operator forwards `string(route.API)` verbatim, so a lowercase union here
 * would match nothing the operator can send: the runner would fall through to
 * the other branch and post an OpenAI body at the Anthropic endpoint, against a
 * gateway that reports healthy.
 *
 * No type system spans this seam — the value crosses a JSON boundary between two
 * languages — so scripts/check-route-api-parity.py holds the declarations
 * together instead, comparing this union against the generated CRD enum, the Go
 * marker behind it, and the zod enum in `@eks-agent/core`.
 */
export type RouteAPI = 'Anthropic' | 'OpenAI';

/**
 * Output ceiling per call. Both wire formats require one and reject a request
 * without it before the model is reached.
 */
const DEFAULT_MAX_TOKENS = 1024;

/** The native Anthropic Messages response, for a route published as Anthropic. */
interface AnthropicResponse {
  model?: string;
  stop_reason?: string | null;
  /** Content is a BLOCK ARRAY, not a string. */
  content?: { type?: string; text?: string }[];
  usage?: {
    input_tokens?: number;
    output_tokens?: number;
    cache_read_input_tokens?: number;
    cache_creation_input_tokens?: number;
  };
  /** Bedrock stamps this on a response its guardrail acted on. */
  'amazon-bedrock-guardrailAction'?: string;
}

/** The OpenAI chat-completions response, for a route published as OpenAI. */
interface OpenAIResponse {
  model?: string;
  choices?: { message?: { content?: string | null }; finish_reason?: string | null }[];
  usage?: {
    prompt_tokens?: number;
    completion_tokens?: number;
    prompt_tokens_details?: { cached_tokens?: number };
  };
}

/** What both parsers reduce to, so invoke() below is format-agnostic. */
interface NormalizedResponse {
  output: string;
  guardrailBlocked: boolean;
  modelId?: string;
  stopReason?: StopReason;
  tokens?: {
    inputTokens: number;
    outputTokens: number;
    cacheReadTokens: number;
    cacheWriteTokens: number;
  };
}

export interface GatewayBackendOptions {
  /**
   * The base URL published on ModelGateway.status.routes[].baseURL, e.g.
   * http://<platform>-gateway.tenants-<platform>.svc.cluster.local:8080/anthropic
   *
   * Not status.endpoint. The gateway serves each wire format under its own
   * prefix, and a request to the bare root reaches no body processor, gets no
   * x-ai-eg-model header and matches no route rule.
   */
  baseURL: string;
  /**
   * The route name published on status.routes[].name. This is what goes in the
   * body's `model` field — a Bedrock model id there matches no rule, because
   * the gateway's own modelNameOverride does that substitution upstream.
   */
  route: string;
  /** The wire format published on status.routes[].api. */
  api: RouteAPI;
  /** Output ceiling per call. Defaults to DEFAULT_MAX_TOKENS. */
  maxTokens?: number;
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
 * Drive a case through the tenant gateway. Reuses the SDK's cost accounting
 * ({@link priceModel}, unpriced-aware) and error taxonomy ({@link AgentError})
 * rather than reimplementing them. The caller (runCases) owns the deadline and
 * threads its AbortSignal in; a gateway non-2xx becomes a classified
 * AgentError, a guardrail intervention becomes a normal (blocked) result.
 */
export class GatewayBackend implements ModelBackend {
  private readonly baseURL: string;
  private readonly route: string;
  private readonly api: RouteAPI;
  private readonly maxTokens: number;
  private readonly fetchImpl: typeof fetch;

  constructor(opts: GatewayBackendOptions) {
    this.baseURL = trimTrailingSlashes(opts.baseURL);
    this.route = opts.route;
    this.api = opts.api;
    this.maxTokens = opts.maxTokens ?? DEFAULT_MAX_TOKENS;
    this.fetchImpl = opts.fetchImpl ?? fetch;
  }

  private url(): string {
    // The path each wire format is served at, appended to the published base —
    // the same suffixes RouteStatus.BaseURL documents and the official SDKs
    // append.
    return this.api === 'Anthropic'
      ? `${this.baseURL}/v1/messages`
      : `${this.baseURL}/chat/completions`;
  }

  private requestBody(input: string): unknown {
    // `model` is the ROUTE NAME. Envoy AI Gateway's extproc reads it out of the
    // body to derive x-ai-eg-model, which is the header the AIGatewayRoute rule
    // matches; modelNameOverride swaps in the Bedrock identifier upstream.
    // Both wire formats accept this same envelope.
    return {
      model: this.route,
      max_tokens: this.maxTokens,
      messages: [{ role: 'user', content: input }],
    };
  }

  async invoke(inv: {
    name: string;
    input: string;
    correlationId: string;
    signal: AbortSignal;
  }): Promise<InvocationResult> {
    const started = Date.now();
    let res: Response;
    try {
      res = await this.fetchImpl(this.url(), {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          'x-correlation-id': inv.correlationId,
        },
        body: JSON.stringify(this.requestBody(inv.input)),
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

    const raw = (await res.json()) as unknown;
    const norm =
      this.api === 'Anthropic'
        ? parseAnthropic(raw as AnthropicResponse)
        : parseOpenAI(raw as OpenAIResponse);

    // Price the call when the response carried both a resolved model and usage.
    // Anything short of that is unpriced — surfaced, never silently $0.
    let costUsd = 0;
    let unpriced = true;
    if (norm.modelId !== undefined && norm.tokens !== undefined) {
      const priced = priceModel({ modelId: norm.modelId, tokens: norm.tokens });
      costUsd = priced.costUsd;
      unpriced = !priced.priced;
    }

    return {
      output: norm.output,
      latencyMs: Date.now() - started,
      costUsd,
      unpriced,
      guardrailBlocked: norm.guardrailBlocked,
      ...(norm.stopReason !== undefined ? { stopReason: norm.stopReason } : {}),
      ...(norm.modelId !== undefined ? { modelId: norm.modelId } : {}),
    };
  }
}

/**
 * Read the native Anthropic Messages response.
 *
 * Bedrock reports a guardrail intervention on the response body rather than as
 * a stop reason, so it is checked first — the platform's StopReason union has
 * `guardrail_intervened` and nothing in this repo was producing it, which left
 * every expectRefusal case graded on refusal-phrase matching alone.
 */
function parseAnthropic(body: AnthropicResponse): NormalizedResponse {
  const output = (body.content ?? [])
    .filter((b) => b.type === 'text')
    .map((b) => b.text ?? '')
    .join('');
  const intervened = body['amazon-bedrock-guardrailAction'] === 'INTERVENED';
  const stopReason: StopReason | undefined = intervened
    ? 'guardrail_intervened'
    : coerceStopReason(body.stop_reason ?? undefined);
  const u = body.usage;
  return {
    output,
    guardrailBlocked: intervened,
    ...(body.model !== undefined ? { modelId: body.model } : {}),
    ...(stopReason !== undefined ? { stopReason } : {}),
    ...(u !== undefined
      ? {
          tokens: {
            inputTokens: u.input_tokens ?? 0,
            outputTokens: u.output_tokens ?? 0,
            cacheReadTokens: u.cache_read_input_tokens ?? 0,
            cacheWriteTokens: u.cache_creation_input_tokens ?? 0,
          },
        }
      : {}),
  };
}

/**
 * OpenAI finish reasons, mapped onto the platform's StopReason union. A Map
 * rather than an object literal: a keyed object read with a response-supplied
 * string is an injection shape the linter is right to flag.
 */
const OPENAI_FINISH = new Map<string, StopReason>([
  ['stop', 'end_turn'],
  ['length', 'max_tokens'],
  ['tool_calls', 'tool_use'],
  ['function_call', 'tool_use'],
  ['content_filter', 'guardrail_intervened'],
]);

/**
 * Read the OpenAI chat-completions response.
 *
 * `usage.prompt_tokens` is the TOTAL prompt count and already includes
 * `prompt_tokens_details.cached_tokens`. Anthropic's fields are disjoint —
 * `input_tokens` excludes both cache counters — and priceModel sums its four
 * terms, so the cached count is subtracted here and not in parseAnthropic.
 * Getting this wrong inflates every cost, which is what `maxCostUsd` grades.
 */
function parseOpenAI(body: OpenAIResponse): NormalizedResponse {
  const choice = body.choices?.[0];
  const finish = choice?.finish_reason ?? undefined;
  const stopReason = finish === undefined ? undefined : (OPENAI_FINISH.get(finish) ?? 'other');
  const u = body.usage;
  const cached = u?.prompt_tokens_details?.cached_tokens ?? 0;
  return {
    output: choice?.message?.content ?? '',
    guardrailBlocked: stopReason === 'guardrail_intervened',
    ...(body.model !== undefined ? { modelId: body.model } : {}),
    ...(stopReason !== undefined ? { stopReason } : {}),
    ...(u !== undefined
      ? {
          tokens: {
            inputTokens: Math.max(0, (u.prompt_tokens ?? 0) - cached),
            outputTokens: u.completion_tokens ?? 0,
            cacheReadTokens: cached,
            cacheWriteTokens: 0,
          },
        }
      : {}),
  };
}

/**
 * Strip trailing slashes from the gateway base URL without a backtracking
 * regex. A quantifier-before-anchor pattern (`/\/+$/`) is a polynomial-ReDoS
 * shape; this linear scan is not.
 */
function trimTrailingSlashes(s: string): string {
  let end = s.length;
  while (end > 0 && s.charCodeAt(end - 1) === 47 /* '/' */) end--;
  return s.slice(0, end);
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
