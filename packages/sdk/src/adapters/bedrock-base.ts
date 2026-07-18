import {
  BedrockRuntimeClient,
  InvokeModelCommand,
  InvokeModelWithResponseStreamCommand,
  ThrottlingException,
  ModelTimeoutException,
  ModelErrorException,
  ResourceNotFoundException,
  ValidationException,
  AccessDeniedException,
  InternalServerException,
  ServiceUnavailableException,
  ServiceQuotaExceededException,
  ModelStreamErrorException,
} from '@aws-sdk/client-bedrock-runtime';
import { fromNodeProviderChain } from '@aws-sdk/credential-providers';
import {
  AgentError,
  type CallEvent,
  type ErrorClass,
  type ModelFamily,
  type TokenUsage,
} from '@eks-agent/core';
import { priceModel } from '@eks-agent/pricing';

import type {
  MessagesParams,
  MessagesResponse,
  ProviderAdapter,
  ProviderId,
  StreamHandlers,
} from '../types.js';

/**
 * Per-family reducer over a model's response-stream events. The base adapter
 * decodes each `InvokeModelWithResponseStream` chunk to JSON and feeds it to
 * `push`; the subclass maps the family's event shape onto accumulated text,
 * token usage, and a stop reason. `push` returns any text delta the event
 * carried (empty string when it carried none) so the base can forward it to
 * `StreamHandlers.onText`.
 */
export interface StreamAccumulator {
  push(event: unknown): string;
  result(): { text: string; usage: TokenUsage; stopReason: MessagesResponse['stopReason'] };
}

export interface BedrockAdapterOptions {
  region: string;
  /** Override the default credential chain (defaults to fromNodeProviderChain — IRSA in-cluster). */
  credentials?: BedrockRuntimeClient['config']['credentials'];
  /**
   * Platform name (matches Platform CR .metadata.name). Threaded into every
   * CallEvent emitted via emitCallEvent so cost-by-platform dashboards work
   * out of the box. Required when emitCallEvent is wired; if omitted, the
   * platform field on CallEvents is empty and downstream attribution breaks.
   */
  platform?: string;
  /**
   * Tenant identifier (matches Platform.spec.tenant). Same role as `platform`
   * above — present on every CallEvent for fan-out aggregation.
   */
  tenant?: string;
  /** Optional Workspace ID for finer-grained spend attribution. */
  workspace?: string;
  /**
   * Per-request deadline in ms applied to every InvokeModel call, even when
   * the caller passes no AbortSignal. Guards against a stalled Bedrock socket
   * hanging the call until the OS TCP timeout. Defaults to 60_000. A caller's
   * own params.signal is combined with (not replaced by) this deadline —
   * whichever fires first wins. Raise it for legitimately long generations.
   */
  requestTimeoutMs?: number;
}

/**
 * BedrockAdapter is the common base for every model-family adapter. Subclasses
 * implement `buildRequestBody` and `parseResponseBody` for their wire shape;
 * everything else (auth, retry classification, telemetry, cost) lives here.
 */
export abstract class BedrockAdapter implements ProviderAdapter {
  readonly providerId: ProviderId = 'bedrock';
  abstract readonly modelFamily: ModelFamily;

  protected client: BedrockRuntimeClient;
  protected readonly platform: string;
  protected readonly tenant: string;
  protected readonly workspace?: string;
  protected readonly requestTimeoutMs: number;

  constructor(opts: BedrockAdapterOptions) {
    this.client = new BedrockRuntimeClient({
      region: opts.region,
      credentials: opts.credentials ?? fromNodeProviderChain(),
    });
    this.platform = opts.platform ?? '';
    this.tenant = opts.tenant ?? '';
    if (opts.workspace !== undefined) this.workspace = opts.workspace;
    this.requestTimeoutMs = opts.requestTimeoutMs ?? 60_000;
  }

  protected abstract buildRequestBody(params: MessagesParams): Record<string, unknown>;
  protected abstract parseResponseBody(body: unknown): {
    text: string;
    usage: TokenUsage;
    stopReason: MessagesResponse['stopReason'];
  };
  /**
   * Construct the family's response-stream reducer. Called once per
   * {@link messagesStream} invocation; the base feeds it decoded chunks and
   * reads the accumulated result once the stream terminates.
   */
  protected abstract streamAccumulator(): StreamAccumulator;

  async messages(params: MessagesParams): Promise<MessagesResponse> {
    const started = Date.now();
    try {
      const cmd = new InvokeModelCommand({
        modelId: params.modelId,
        contentType: 'application/json',
        accept: 'application/json',
        body: JSON.stringify(this.buildRequestBody(params)),
        ...this.guardrailFields(params),
      });
      const out = await this.client.send(cmd, { abortSignal: this.deadlineSignal(params) });
      const body = JSON.parse(new TextDecoder().decode(out.body)) as unknown;
      return this.finalize(params, this.parseResponseBody(body), started);
    } catch (err) {
      throw this.asAgentError(params, err);
    }
  }

  async messagesStream(
    params: MessagesParams,
    handlers?: StreamHandlers,
  ): Promise<MessagesResponse> {
    const started = Date.now();
    try {
      const cmd = new InvokeModelWithResponseStreamCommand({
        modelId: params.modelId,
        contentType: 'application/json',
        accept: 'application/json',
        body: JSON.stringify(this.buildRequestBody(params)),
        ...this.guardrailFields(params),
      });
      const out = await this.client.send(cmd, { abortSignal: this.deadlineSignal(params) });
      const acc = this.streamAccumulator();
      if (out.body) {
        for await (const event of out.body) {
          // A mid-stream error surfaces as a modeled union member. Rethrow it
          // so it lands in the catch and is classified by the same taxonomy as
          // a synchronous failure — the SDK's own deserializer throws these
          // too, but checking the members keeps the behavior deterministic.
          const streamErr =
            event.internalServerException ??
            event.modelStreamErrorException ??
            event.throttlingException ??
            event.validationException ??
            event.serviceUnavailableException ??
            event.modelTimeoutException;
          if (streamErr) throw streamErr;
          const bytes = event.chunk?.bytes;
          if (!bytes) continue;
          const delta = acc.push(JSON.parse(new TextDecoder().decode(bytes)));
          if (delta && handlers?.onText) handlers.onText(delta);
        }
      }
      // Cost + CallEvent fire once here, after the stream terminates and the
      // full usage (input + cache + output tokens) is finally known.
      return this.finalize(params, acc.result(), started);
    } catch (err) {
      throw this.asAgentError(params, err);
    }
  }

  /** Guardrail identifier/version fields, present only when a guardrail is set. */
  private guardrailFields(
    params: MessagesParams,
  ): { guardrailIdentifier: string; guardrailVersion: string } | Record<string, never> {
    if (!params.guardrailId) return {};
    return {
      guardrailIdentifier: params.guardrailId,
      guardrailVersion: params.guardrailVersion ?? 'DRAFT',
    };
  }

  /**
   * Every Bedrock call gets a bounded deadline regardless of caller diligence:
   * a default request timeout is always applied, and a caller-supplied
   * AbortSignal is combined with it (earliest fire wins) so both in-flight
   * cancellation and the safety-net deadline hold. The default-deadline fire
   * surfaces as a TimeoutError (classified Network, retryable); a caller abort
   * surfaces as AbortError (Cancelled).
   */
  private deadlineSignal(params: MessagesParams): AbortSignal {
    const deadline = AbortSignal.timeout(this.requestTimeoutMs);
    return params.signal ? AbortSignal.any([params.signal, deadline]) : deadline;
  }

  /**
   * Price the parsed result, assemble the MessagesResponse, and emit the
   * success CallEvent. Shared by the synchronous and streaming paths so cost
   * accounting and telemetry are identical whichever transport was used.
   */
  private finalize(
    params: MessagesParams,
    parsed: { text: string; usage: TokenUsage; stopReason: MessagesResponse['stopReason'] },
    started: number,
  ): MessagesResponse {
    const { costUsd, priced } = priceModel({ modelId: params.modelId, tokens: parsed.usage });
    const response: MessagesResponse = {
      text: parsed.text,
      stopReason: parsed.stopReason,
      usage: parsed.usage,
      costUsd,
      latencyMs: Date.now() - started,
      correlationId: params.correlationId,
    };
    // Telemetry hook — subclasses or DI can wire emitCallEvent to push to
    // OTel / Datadog. We only call it on success; failures throw and the
    // caller sees the AgentError directly.
    if (this.emitCallEvent) {
      this.emitCallEvent({
        correlationId: params.correlationId,
        platform: this.platform,
        tenant: this.tenant,
        modelFamily: this.modelFamily,
        modelId: params.modelId,
        tokens: parsed.usage,
        costUsd,
        ...(priced ? {} : { unpriced: true }),
        latencyMs: response.latencyMs,
        status: 'ok',
        timestamp: new Date().toISOString(),
        ...(this.workspace !== undefined ? { workspace: this.workspace } : {}),
      });
    }
    return response;
  }

  /**
   * Normalize a thrown error into an AgentError. Don't double-wrap: if a
   * subclass's parseResponseBody already threw an AgentError with a precise
   * classification (e.g. a response schema-validation failure), preserve its
   * class, message, and cause — only backfilling the correlation id from the
   * call params when the subclass threw before it was attached, so every
   * surfaced error stays correlation-tagged.
   */
  private asAgentError(params: MessagesParams, err: unknown): AgentError {
    if (err instanceof AgentError) {
      if (err.correlationId === undefined) {
        return new AgentError({
          class: err.class,
          message: err.message,
          cause: err.cause,
          correlationId: params.correlationId,
        });
      }
      return err;
    }
    return new AgentError({
      class: this.classifyError(err),
      message: err instanceof Error ? err.message : String(err),
      cause: err,
      correlationId: params.correlationId,
    });
  }

  emitCallEvent?(event: CallEvent): void;

  classifyError(err: unknown): ErrorClass {
    if (err instanceof ThrottlingException || err instanceof ServiceQuotaExceededException)
      return 'RateLimit';
    if (err instanceof ServiceUnavailableException) return 'Overloaded';
    if (err instanceof ValidationException || err instanceof ResourceNotFoundException)
      return 'BadRequest';
    if (err instanceof AccessDeniedException) return 'AuthFailure';
    if (
      err instanceof InternalServerException ||
      err instanceof ModelErrorException ||
      err instanceof ModelStreamErrorException
    )
      return 'Server';
    if (err instanceof ModelTimeoutException) return 'Network';
    if (err && typeof err === 'object' && 'name' in err) {
      const name = String(err.name);
      if (name === 'AbortError') return 'Cancelled';
      // The default request-deadline fires as a TimeoutError (vs a caller
      // AbortError); a timeout is a transient network condition, so it is
      // retryable, not a deliberate cancellation.
      if (name === 'TimeoutError') return 'Network';
      if (name === 'GuardrailIntervenedException') return 'GuardrailBlock';
    }
    return 'Server';
  }
}
