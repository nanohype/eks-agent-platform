/**
 * The message vocabulary shared by anything that calls a model through a
 * Platform's ModelGateway.
 *
 * These are plain types rather than zod schemas: they describe a request a
 * caller builds and a field it reads back, not a CR the client parses off the
 * API server. The zod schemas in `schemas.ts` are the read-boundary contract
 * for cluster resources and are gated against the CRDs; these are not, and the
 * drift gate deliberately does not see them.
 */

/** One turn in a model conversation. */
export interface Message {
  role: 'system' | 'user' | 'assistant';
  content: string;
  /**
   * Place a prompt-cache breakpoint at the end of this message.
   *
   * The stable-prefix idiom: put the unchanging part of the prompt — the
   * system instructions and any large, reused context (tool schemas, a
   * retrieved corpus, few-shot examples) — FIRST, and mark the last stable
   * message. Everything up to and including the breakpoint is cached; a later
   * call with the same prefix reads it back at the cache-read price instead of
   * re-billing full input tokens. Only mark content that repeats verbatim
   * across calls — a breakpoint in front of the per-request tail never hits.
   *
   * The gateway renders this into whatever the route's wire format calls it.
   * A route whose model has no prompt-cache surface ignores the flag.
   */
  cache?: boolean;
}

/**
 * Why a model stopped generating.
 *
 * `guardrail_intervened` is the one that is not a model-side outcome: the
 * route's guardrail blocked the exchange. It is kept in the same union rather
 * than signalled separately so every caller has to account for it in the same
 * place it accounts for `end_turn` — a blocked call that classifies as a
 * normal completion is the failure this prevents.
 */
export type StopReason =
  | 'end_turn'
  | 'max_tokens'
  | 'stop_sequence'
  | 'tool_use'
  | 'guardrail_intervened'
  | 'other';
