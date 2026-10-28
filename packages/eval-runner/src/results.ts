import { z } from 'zod';

import type { CaseResult, StopReason } from './types.js';

/**
 * Every {@link StopReason}, keyed by itself. The mapped type requires each
 * member and the object literal admits no other key, so a member added to the
 * union in `@eks-agent/core` fails the build here instead of failing `score` on
 * the first result that carries it.
 */
const STOP_REASONS = {
  end_turn: 'end_turn',
  max_tokens: 'max_tokens',
  stop_sequence: 'stop_sequence',
  tool_use: 'tool_use',
  guardrail_intervened: 'guardrail_intervened',
  other: 'other',
} as const satisfies { [K in StopReason]: K };

/**
 * Runtime schema for one `results.json` record. `evaluate` writes the file and
 * `score` reads it in a later workflow step, after it has passed through an
 * Argo output parameter, so `score` parses it here rather than asserting its
 * type: a record missing a field fails with that field's path, where a cast
 * would grade the case against `undefined`.
 *
 * Strict, because `evaluate` is the only writer: a key this schema does not
 * know means the writer and the reader come from different builds.
 */
export const CaseResultSchema = z
  .object({
    name: z.string(),
    input: z.string(),
    output: z.string(),
    latency_ms: z.number(),
    cost_usd: z.number(),
    unpriced: z.boolean(),
    guardrailBlocked: z.boolean(),
    stopReason: z.enum(STOP_REASONS).exactOptional(),
    error: z.string().exactOptional(),
    expectContains: z.array(z.string()).exactOptional(),
    expectNotContains: z.array(z.string()).exactOptional(),
    expectRefusal: z.boolean().exactOptional(),
    maxLatencyMs: z.number().exactOptional(),
    maxCostUsd: z.string().exactOptional(),
  })
  .strict() satisfies z.ZodType<CaseResult>;

export const CaseResultsSchema = z.array(CaseResultSchema);

/**
 * Parse and validate a `results.json` document. Throws a `ZodError` naming the
 * offending record and field when the document does not match what `evaluate`
 * writes.
 */
export function parseResults(json: string): CaseResult[] {
  const raw: unknown = JSON.parse(json);
  return CaseResultsSchema.parse(raw);
}
