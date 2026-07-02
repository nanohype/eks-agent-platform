import type { ModelFamily, TokenUsage } from '@eks-agent/core';

import type { MessagesParams, MessagesResponse } from '../types.js';

import { BedrockAdapter } from './bedrock-base.js';

interface NovaResponse {
  output?: { message?: { content?: { text?: string }[] } };
  stopReason?: 'end_turn' | 'max_tokens' | 'stop_sequence' | 'tool_use';
  usage?: { inputTokens: number; outputTokens: number };
}

export class NovaBedrockAdapter extends BedrockAdapter {
  readonly modelFamily: ModelFamily = 'amazon-nova';

  protected buildRequestBody(params: MessagesParams): Record<string, unknown> {
    const system = params.messages
      .filter((m) => m.role === 'system')
      .map((m) => ({ text: m.content }));
    const messages = params.messages
      .filter((m) => m.role !== 'system')
      .map((m) => ({ role: m.role, content: [{ text: m.content }] }));
    return {
      schemaVersion: 'messages-v1',
      ...(system.length ? { system } : {}),
      messages,
      inferenceConfig: {
        maxTokens: params.maxTokens,
        ...(params.temperature !== undefined ? { temperature: params.temperature } : {}),
        ...(params.stop ? { stopSequences: params.stop } : {}),
      },
    };
  }

  protected parseResponseBody(body: unknown): {
    text: string;
    usage: TokenUsage;
    stopReason: MessagesResponse['stopReason'];
  } {
    const r = body as NovaResponse;
    const text = r.output?.message?.content?.map((c) => c.text ?? '').join('') ?? '';
    const usage: TokenUsage = {
      inputTokens: r.usage?.inputTokens ?? 0,
      outputTokens: r.usage?.outputTokens ?? 0,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
    };
    return { text, usage, stopReason: r.stopReason ?? 'end_turn' };
  }
}
