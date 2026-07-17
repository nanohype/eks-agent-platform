import type { ModelFamily, TokenUsage } from '@eks-agent/core';

import type { Message, MessagesParams, MessagesResponse } from '../types.js';

import { BedrockAdapter, type StreamAccumulator } from './bedrock-base.js';

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

// A cache_control breakpoint on a system/content block. On Bedrock's native
// Anthropic InvokeModel format this is the prompt-cache marker (the InvokeModel
// equivalent of the Converse `cachePoint` block).
const ephemeralCacheControl = { type: 'ephemeral' } as const;

// Render one message's content: a plain string normally, or a single
// cache-marked text block when the message opens a prompt-cache breakpoint.
function anthropicContent(
  m: Message,
): string | { type: 'text'; text: string; cache_control?: typeof ephemeralCacheControl }[] {
  if (!m.cache) return m.content;
  return [{ type: 'text', text: m.content, cache_control: ephemeralCacheControl }];
}

export class AnthropicBedrockAdapter extends BedrockAdapter {
  readonly modelFamily: ModelFamily = 'anthropic';

  protected buildRequestBody(params: MessagesParams): Record<string, unknown> {
    const systemMsgs = params.messages.filter((m) => m.role === 'system');
    const others = params.messages.filter((m) => m.role !== 'system');
    const systemText = systemMsgs.map((m) => m.content).join('\n\n');
    // Cache the whole system prefix when any system message asks for it. The
    // system field stays a plain string in the common (uncached) case; it
    // becomes a one-block array carrying cache_control only when caching is on,
    // so the stable-prefix breakpoint lands exactly where the caller marked it.
    const systemCached = systemMsgs.some((m) => m.cache);
    return {
      anthropic_version: 'bedrock-2023-05-31',
      max_tokens: params.maxTokens,
      ...(systemText
        ? {
            system: systemCached
              ? [{ type: 'text', text: systemText, cache_control: ephemeralCacheControl }]
              : systemText,
          }
        : {}),
      messages: others.map((m) => ({
        role: m.role as 'user' | 'assistant',
        content: anthropicContent(m),
      })),
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

  protected streamAccumulator(): StreamAccumulator {
    return new AnthropicStreamAccumulator();
  }
}

// Anthropic streaming event shapes (Bedrock InvokeModelWithResponseStream, one
// decoded JSON object per chunk):
//   message_start        → usage.input_tokens + cache_{read,creation}_input_tokens
//   content_block_delta  → delta.text (a text fragment)
//   message_delta        → usage.output_tokens, delta.stop_reason
//   message_stop         → terminal (Bedrock also attaches invocation metrics)
interface AnthropicStreamEvent {
  type?: string;
  message?: {
    usage?: {
      input_tokens?: number;
      output_tokens?: number;
      cache_read_input_tokens?: number;
      cache_creation_input_tokens?: number;
    };
  };
  delta?: { type?: string; text?: string; stop_reason?: string };
  usage?: { output_tokens?: number };
  'amazon-bedrock-invocationMetrics'?: {
    inputTokenCount?: number;
    outputTokenCount?: number;
    cacheReadInputTokenCount?: number;
    cacheWriteInputTokenCount?: number;
  };
}

const STOP_REASONS: ReadonlySet<string> = new Set([
  'end_turn',
  'max_tokens',
  'stop_sequence',
  'tool_use',
]);

class AnthropicStreamAccumulator implements StreamAccumulator {
  private text = '';
  private usage: TokenUsage = {
    inputTokens: 0,
    outputTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
  };
  private stopReason: MessagesResponse['stopReason'] = 'end_turn';

  push(event: unknown): string {
    const e = event as AnthropicStreamEvent;
    if (e.type === 'message_start' && e.message?.usage) {
      const u = e.message.usage;
      this.usage.inputTokens = u.input_tokens ?? this.usage.inputTokens;
      this.usage.cacheReadTokens = u.cache_read_input_tokens ?? this.usage.cacheReadTokens;
      this.usage.cacheWriteTokens = u.cache_creation_input_tokens ?? this.usage.cacheWriteTokens;
    }
    if (e.type === 'content_block_delta' && e.delta?.type === 'text_delta' && e.delta.text) {
      this.text += e.delta.text;
      return e.delta.text;
    }
    if (e.type === 'message_delta') {
      if (e.usage?.output_tokens !== undefined) this.usage.outputTokens = e.usage.output_tokens;
      if (e.delta?.stop_reason && STOP_REASONS.has(e.delta.stop_reason)) {
        this.stopReason = e.delta.stop_reason as MessagesResponse['stopReason'];
      }
    }
    // Bedrock appends authoritative token counts on the final chunk; prefer
    // them so cache tokens are correct even if an intermediate event lagged.
    const m = e['amazon-bedrock-invocationMetrics'];
    if (m) {
      if (m.inputTokenCount !== undefined) this.usage.inputTokens = m.inputTokenCount;
      if (m.outputTokenCount !== undefined) this.usage.outputTokens = m.outputTokenCount;
      if (m.cacheReadInputTokenCount !== undefined)
        this.usage.cacheReadTokens = m.cacheReadInputTokenCount;
      if (m.cacheWriteInputTokenCount !== undefined)
        this.usage.cacheWriteTokens = m.cacheWriteInputTokenCount;
    }
    return '';
  }

  result(): { text: string; usage: TokenUsage; stopReason: MessagesResponse['stopReason'] } {
    return { text: this.text, usage: this.usage, stopReason: this.stopReason };
  }
}
