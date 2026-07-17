/**
 * bedrock-rag — a retrieval-augmented tenant client on `@eks-agent/sdk`.
 *
 * Three SDK features you'd fold into a real RAG worker:
 *   1. Prompt caching (`cachePoint`) on the stable retrieved corpus, so repeat
 *      questions over the same context read the prefix back at the cache-read
 *      price instead of re-billing full input tokens.
 *   2. A `[primary, fallback]` model router that walks to Haiku when Sonnet is
 *      throttled or overloaded, and throws a typed `ChainExhaustedError` only
 *      when the whole chain fails.
 *   3. Streaming token deltas.
 *
 * Nothing here talks to a cluster: it builds the same Bedrock calls a pod
 * running under the tenant's Pod Identity role would make. Credentials come
 * from the default provider chain (in-cluster, that's the tenant role).
 */
import type { CallEvent } from '@eks-agent/core';
import { ChainExhaustedError, createBedrockAdapter, createModelRouter } from '@eks-agent/sdk';
import type { Message, MessagesResponse, ModelRouter, RouteTarget } from '@eks-agent/sdk';

/** Sonnet 4.6 primary. */
const PRIMARY: RouteTarget = {
  modelFamily: 'anthropic',
  modelId: 'us.anthropic.claude-sonnet-4-6',
};

/** Haiku 4.5 fallback — tried when the primary fails with a non-terminal error. */
const FALLBACK: RouteTarget = {
  modelFamily: 'anthropic',
  modelId: 'us.anthropic.claude-haiku-4-5-20251001-v1:0',
};

const CHAIN: RouteTarget[] = [PRIMARY, FALLBACK];

export interface RagConfig {
  /** AWS region the Bedrock models are invoked in. */
  region: string;
  /** Platform CR name — stamped onto every emitted CallEvent for cost attribution. */
  platform: string;
  /** Owning team (Platform.spec.tenant) — same role on CallEvents. */
  tenant: string;
  /** Optional sink for per-attempt cost/usage events (including failed rungs). */
  onCallEvent?: (event: CallEvent) => void;
}

/**
 * Build the fallback router over `[Sonnet, Haiku]`. The primary answers unless
 * it fails with a retryable error, at which point the router advances to the
 * fallback and (when wired) emits an error CallEvent for the failed rung.
 */
export function buildRouter(cfg: RagConfig): ModelRouter {
  return createModelRouter(CHAIN, {
    region: cfg.region,
    platform: cfg.platform,
    tenant: cfg.tenant,
    ...(cfg.onCallEvent ? { onCallEvent: cfg.onCallEvent } : {}),
  });
}

/**
 * Assemble the prompt so the expensive, reused part — the system instruction
 * and the retrieved corpus — sits at the FRONT with a cache breakpoint, and the
 * per-request question is the uncached tail. `cache: true` marks the last stable
 * message; Bedrock caches every input token up to and including it.
 */
function ragMessages(corpus: string, question: string): Message[] {
  return [
    {
      role: 'system',
      content:
        'You answer strictly from the provided context. Cite the source ids you use. ' +
        "If the answer is not in the context, say you don't know.",
    },
    {
      role: 'user',
      content: `Context:\n${corpus}`,
      cache: true, // stable prefix — everything up to here is cacheable
    },
    {
      role: 'user',
      content: `Question: ${question}`,
    },
  ];
}

/**
 * Answer a question over a retrieved corpus through the fallback router with a
 * cached corpus prefix. Rejects with {@link ChainExhaustedError} if every rung
 * of the chain fails; narrow it with {@link isChainExhausted}.
 */
export async function answerFromContext(
  router: ModelRouter,
  corpus: string,
  question: string,
  correlationId: string,
): Promise<MessagesResponse> {
  return router.messages({
    messages: ragMessages(corpus, question),
    maxTokens: 512,
    correlationId,
  });
}

/**
 * Stream the answer token-by-token. The router has no streaming surface (falling
 * back across a half-emitted stream isn't well-defined), so streaming pins the
 * primary model directly through a single adapter. Resolves with the full
 * accumulated response once the stream terminates.
 */
export async function streamAnswer(
  cfg: RagConfig,
  corpus: string,
  question: string,
  correlationId: string,
  onDelta: (text: string) => void,
): Promise<MessagesResponse> {
  const adapter = createBedrockAdapter(PRIMARY.modelFamily, {
    region: cfg.region,
    platform: cfg.platform,
    tenant: cfg.tenant,
  });
  return adapter.messagesStream(
    {
      modelId: PRIMARY.modelId,
      modelFamily: PRIMARY.modelFamily,
      messages: ragMessages(corpus, question),
      maxTokens: 512,
      correlationId,
    },
    { onText: onDelta },
  );
}

/** Narrow a thrown value to the router's typed chain-exhaustion failure. */
export function isChainExhausted(err: unknown): err is ChainExhaustedError {
  return err instanceof ChainExhaustedError;
}
