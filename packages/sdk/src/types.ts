import type { ErrorClass, TokenUsage, ModelFamily, CallEvent } from '@eks-agent/core';

export type ProviderId = 'bedrock';

export interface Message {
  role: 'system' | 'user' | 'assistant';
  content: string;
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
  stopReason: 'end_turn' | 'max_tokens' | 'stop_sequence' | 'tool_use' | 'guardrail_intervened' | 'other';
  usage: TokenUsage;
  costUsd: number;
  latencyMs: number;
  correlationId: string;
}

export interface ProviderAdapter {
  readonly providerId: ProviderId;
  readonly modelFamily: ModelFamily;
  messages(params: MessagesParams): Promise<MessagesResponse>;
  classifyError(err: unknown): ErrorClass;
  /**
   * Optional hook for emitting structured call events to OTel /
   * Datadog / etc. When set, BedrockAdapter.messages() invokes it
   * after every successful call.
   */
  emitCallEvent?(event: CallEvent): void;
}
