import type { ErrorClass, TokenUsage, ModelFamily, CallEvent } from '@eks-agent/core';

export type ProviderId = 'bedrock';

export interface Message {
  role: 'system' | 'user' | 'assistant';
  content: string;
  /**
   * Place a Bedrock prompt-cache breakpoint at the end of this message.
   *
   * The stable-prefix idiom: put the unchanging part of the prompt — the
   * system instructions and any large, reused context (tool schemas, a
   * retrieved corpus, few-shot examples) — FIRST, and mark the last stable
   * message with `cache: true`. Bedrock caches every input token up to and
   * including that breakpoint; a later call with the same prefix reads it back
   * at the cache-read price instead of re-billing full input tokens. Only mark
   * content that repeats verbatim across calls — a breakpoint in front of the
   * per-request tail (the user's turn) never hits.
   *
   * On the Anthropic families this renders as `cache_control: {type:
   * 'ephemeral'}` on the corresponding system/content block (the InvokeModel
   * form of the Converse `cachePoint` marker). Families without a Bedrock
   * prompt-cache surface ignore the flag.
   */
  cache?: boolean;
}

/**
 * Callbacks for a streaming call. `onText` fires once per text delta as the
 * model emits it; the returned {@link MessagesResponse} still carries the full
 * accumulated text, final usage, and cost once the stream completes.
 */
export interface StreamHandlers {
  onText?(delta: string): void;
}

export interface MessagesParams {
  modelId: string;
  modelFamily: ModelFamily;
  messages: Message[];
  maxTokens: number;
  temperature?: number;
  stop?: string[];
  correlationId: string;
  guardrailId?: string;
  guardrailVersion?: string;
  /**
   * Cancellation token. Caller passes an AbortController.signal; the adapter
   * threads it into the underlying SDK Send call so InvokeModelCommand is
   * aborted in-flight when the controller fires. The thrown AgentError is
   * classified as 'Cancelled' (not retryable).
   */
  signal?: AbortSignal;
}

export interface MessagesResponse {
  text: string;
  stopReason:
    | 'end_turn'
    | 'max_tokens'
    | 'stop_sequence'
    | 'tool_use'
    | 'guardrail_intervened'
    | 'other';
  usage: TokenUsage;
  costUsd: number;
  latencyMs: number;
  correlationId: string;
}

export interface ProviderAdapter {
  readonly providerId: ProviderId;
  readonly modelFamily: ModelFamily;
  messages(params: MessagesParams): Promise<MessagesResponse>;
  /**
   * Streaming counterpart of {@link messages}. Sends
   * InvokeModelWithResponseStream, surfaces text deltas via
   * `handlers.onText`, and resolves with the full accumulated response once
   * the stream terminates. Same auth, request-deadline, error taxonomy, and
   * cost accounting as {@link messages}; the CallEvent (when emitCallEvent is
   * wired) fires once, on the final usage after the stream completes.
   */
  messagesStream(params: MessagesParams, handlers?: StreamHandlers): Promise<MessagesResponse>;
  classifyError(err: unknown): ErrorClass;
  /**
   * Optional hook for emitting structured call events to OTel /
   * Datadog / etc. When set, BedrockAdapter.messages() invokes it
   * after every successful call.
   */
  emitCallEvent?(event: CallEvent): void;
}
