import { AgentError, type ModelFamily, type TokenUsage } from '@eks-agent/core';
import { z } from 'zod';

import type { MessagesParams, MessagesResponse } from '../types.js';

import { BedrockAdapter, type StreamAccumulator } from './bedrock-base.js';

// Schema for a Bedrock InvokeModel Amazon Nova response body. Fields stay
// optional (the parser tolerates a truncated response), but a present field
// with the wrong type — content that is not an array, a string-typed token
// count, a non-object body — fails validation at the adapter boundary and
// surfaces as a typed AgentError instead of a silently-zero token count.
const novaResponseSchema = z.object({
  output: z
    .object({
      message: z
        .object({ content: z.array(z.object({ text: z.string().optional() })).optional() })
        .optional(),
    })
    .optional(),
  stopReason: z.string().optional(),
  usage: z.object({ inputTokens: z.number(), outputTokens: z.number() }).optional(),
});

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
    const parsed = novaResponseSchema.safeParse(body);
    if (!parsed.success) {
      throw new AgentError({
        class: 'Server',
        message: `malformed Amazon Nova Bedrock InvokeModel response: ${parsed.error.message}`,
        cause: parsed.error,
      });
    }
    const r = parsed.data;
    const text = r.output?.message?.content?.map((c) => c.text ?? '').join('') ?? '';
    const usage: TokenUsage = {
      inputTokens: r.usage?.inputTokens ?? 0,
      outputTokens: r.usage?.outputTokens ?? 0,
      cacheReadTokens: 0,
      cacheWriteTokens: 0,
    };
    const stopReason =
      r.stopReason && NOVA_STOP_REASONS.has(r.stopReason)
        ? (r.stopReason as MessagesResponse['stopReason'])
        : 'end_turn';
    return { text, usage, stopReason };
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
