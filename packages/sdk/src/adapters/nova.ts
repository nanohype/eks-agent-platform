import type { ModelFamily, TokenUsage } from '@eks-agent/core';

import type { MessagesParams, MessagesResponse } from '../types.js';

import { BedrockAdapter, type StreamAccumulator } from './bedrock-base.js';

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

  protected streamAccumulator(): StreamAccumulator {
    return new NovaStreamAccumulator();
  }
}

// Nova streaming event shapes (Bedrock InvokeModelWithResponseStream, one
// decoded JSON object per chunk):
//   contentBlockDelta → delta.text (a text fragment)
//   messageStop       → stopReason
//   metadata          → usage.{inputTokens, outputTokens}
interface NovaStreamEvent {
  contentBlockDelta?: { delta?: { text?: string } };
  messageStop?: { stopReason?: string };
  metadata?: { usage?: { inputTokens?: number; outputTokens?: number } };
}

const NOVA_STOP_REASONS: ReadonlySet<string> = new Set([
  'end_turn',
  'max_tokens',
  'stop_sequence',
  'tool_use',
]);

class NovaStreamAccumulator implements StreamAccumulator {
  private text = '';
  private usage: TokenUsage = {
    inputTokens: 0,
    outputTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
  };
  private stopReason: MessagesResponse['stopReason'] = 'end_turn';

  push(event: unknown): string {
    const e = event as NovaStreamEvent;
    if (e.contentBlockDelta?.delta?.text) {
      this.text += e.contentBlockDelta.delta.text;
      return e.contentBlockDelta.delta.text;
    }
    if (e.messageStop?.stopReason && NOVA_STOP_REASONS.has(e.messageStop.stopReason)) {
      this.stopReason = e.messageStop.stopReason as MessagesResponse['stopReason'];
    }
    if (e.metadata?.usage) {
      this.usage.inputTokens = e.metadata.usage.inputTokens ?? this.usage.inputTokens;
      this.usage.outputTokens = e.metadata.usage.outputTokens ?? this.usage.outputTokens;
    }
    return '';
  }

  result(): { text: string; usage: TokenUsage; stopReason: MessagesResponse['stopReason'] } {
    return { text: this.text, usage: this.usage, stopReason: this.stopReason };
  }
}
