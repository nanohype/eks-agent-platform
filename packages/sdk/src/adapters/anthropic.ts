import type { ModelFamily, TokenUsage } from '@eks-agent/core';

import type { MessagesParams, MessagesResponse } from '../types.js';

import { BedrockAdapter } from './bedrock-base.js';

interface AnthropicResponse {
  content?: { type: 'text'; text: string }[];
  stop_reason?: 'end_turn' | 'max_tokens' | 'stop_sequence' | 'tool_use';
  usage?: {
    input_tokens: number;
    output_tokens: number;
    cache_read_input_tokens?: number;
    cache_creation_input_tokens?: number;
  };
  amazon_bedrock_invocation_metrics?: { inputTokenCount: number; outputTokenCount: number };
}

export class AnthropicBedrockAdapter extends BedrockAdapter {
  readonly modelFamily: ModelFamily = 'anthropic';

  protected buildRequestBody(params: MessagesParams): Record<string, unknown> {
    const { system, messages } = this.toAnthropicMessages(params.messages);
    return {
      anthropic_version: 'bedrock-2023-05-31',
      max_tokens: params.maxTokens,
      ...(system ? { system } : {}),
      messages,
      ...(params.temperature !== undefined ? { temperature: params.temperature } : {}),
      ...(params.stop ? { stop_sequences: params.stop } : {}),
    };
  }

  protected parseResponseBody(body: unknown): {
    text: string;
    usage: TokenUsage;
    stopReason: MessagesResponse['stopReason'];
  } {
    const r = body as AnthropicResponse;
    const text = r.content?.map((c) => c.text).join('') ?? '';
    const usage: TokenUsage = {
      inputTokens:
        r.usage?.input_tokens ?? r.amazon_bedrock_invocation_metrics?.inputTokenCount ?? 0,
      outputTokens:
        r.usage?.output_tokens ?? r.amazon_bedrock_invocation_metrics?.outputTokenCount ?? 0,
      cacheReadTokens: r.usage?.cache_read_input_tokens ?? 0,
      cacheWriteTokens: r.usage?.cache_creation_input_tokens ?? 0,
    };
    return { text, usage, stopReason: r.stop_reason ?? 'end_turn' };
  }
}
