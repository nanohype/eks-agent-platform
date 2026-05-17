import {
  BedrockRuntimeClient,
  InvokeModelCommand,
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
import { AgentError, type CallEvent, type ErrorClass, type ModelFamily, type TokenUsage } from '@eks-agent/core';
import { estimateCost } from '@eks-agent/pricing';

import type { Message, MessagesParams, MessagesResponse, ProviderAdapter, ProviderId } from '../types.js';

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

  constructor(opts: BedrockAdapterOptions) {
    this.client = new BedrockRuntimeClient({
      region: opts.region,
      credentials: opts.credentials ?? fromNodeProviderChain(),
    });
    this.platform = opts.platform ?? '';
    this.tenant = opts.tenant ?? '';
    if (opts.workspace !== undefined) this.workspace = opts.workspace;
  }

  protected abstract buildRequestBody(params: MessagesParams): Record<string, unknown>;
  protected abstract parseResponseBody(body: unknown): {
    text: string;
    usage: TokenUsage;
    stopReason: MessagesResponse['stopReason'];
  };

  async messages(params: MessagesParams): Promise<MessagesResponse> {
    const started = Date.now();
    try {
      const cmd = new InvokeModelCommand({
        modelId: params.modelId,
        contentType: 'application/json',
        accept: 'application/json',
        body: JSON.stringify(this.buildRequestBody(params)),
        ...(params.guardrailId
          ? {
              guardrailIdentifier: params.guardrailId,
              guardrailVersion: params.guardrailVersion ?? 'DRAFT',
            }
          : {}),
      });

      // Thread the caller's AbortSignal through to the AWS SDK send call so
      // InvokeModelCommand is cancelled in-flight, not just unwaited.
      // exactOptionalPropertyTypes forces a conditional spread rather than
      // `{ abortSignal: signal | undefined }`.
      const out = await this.client.send(cmd, params.signal ? { abortSignal: params.signal } : {});
      const body = JSON.parse(new TextDecoder().decode(out.body)) as unknown;
      const parsed = this.parseResponseBody(body);
      const costUsd = estimateCost({
        modelId: params.modelId,
        tokens: parsed.usage,
      });
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
        const event: CallEvent = {
          correlationId: params.correlationId,
          platform: this.platform,
          tenant: this.tenant,
          modelFamily: this.modelFamily,
          modelId: params.modelId,
          tokens: parsed.usage,
          costUsd,
          latencyMs: response.latencyMs,
          status: 'ok',
          timestamp: new Date().toISOString(),
          ...(this.workspace !== undefined ? { workspace: this.workspace } : {}),
        };
        this.emitCallEvent(event);
      }

      return response;
    } catch (err) {
      // Don't double-wrap: if a subclass's parseResponseBody already threw
      // an AgentError with a precise classification, preserve it.
      if (err instanceof AgentError) throw err;
      throw new AgentError({
        class: this.classifyError(err),
        message: err instanceof Error ? err.message : String(err),
        cause: err,
        correlationId: params.correlationId,
      });
    }
  }

  emitCallEvent?(event: CallEvent): void;

  classifyError(err: unknown): ErrorClass {
    if (err instanceof ThrottlingException || err instanceof ServiceQuotaExceededException) return 'RateLimit';
    if (err instanceof ServiceUnavailableException) return 'Overloaded';
    if (err instanceof ValidationException || err instanceof ResourceNotFoundException) return 'BadRequest';
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
      if (name === 'GuardrailIntervenedException') return 'GuardrailBlock';
    }
    return 'Server';
  }

  protected toAnthropicMessages(messages: Message[]): {
    system?: string;
    messages: { role: 'user' | 'assistant'; content: string }[];
  } {
    const sys = messages
      .filter((m) => m.role === 'system')
      .map((m) => m.content)
      .join('\n\n');
    const others = messages
      .filter((m) => m.role !== 'system')
      .map((m) => ({ role: m.role as 'user' | 'assistant', content: m.content }));
    return { ...(sys ? { system: sys } : {}), messages: others };
  }
}
