import { z } from 'zod';

import type { EvalCase } from './types.js';

/**
 * Runtime schema for a case as it arrives over the wire — the JSON the
 * operator's `buildInlineCasesParam` emits, or an S3 case manifest. Parsing at
 * the read boundary (rather than casting) means a malformed suite fails with a
 * precise error at ingest instead of a `undefined is not a function` deep in
 * scoring.
 */
export const EvalCaseSchema = z
  .object({
    name: z.string().min(1),
    input: z.string(),
    // The operator's `buildInlineCasesParam` serializes every field
    // unconditionally, so a case with no positive assertion arrives as
    // `expectContains: null` (Go marshals a nil slice to null). Accept null as
    // "absent" rather than rejecting the operator's own wire shape.
    expectContains: z.array(z.string()).nullish(),
    expectNotContains: z.array(z.string()).nullish(),
    expectRefusal: z.boolean().nullish(),
    maxLatencyMs: z.number().int().nonnegative().nullish(),
    maxCostUsd: z.string().nullish(),
  })
  .strict();

export const EvalCasesSchema = z.array(EvalCaseSchema);

/**
 * Parse and validate a `cases.json` document. Drops the fields the operator
 * serializes as empty defaults (`expectContains: []`, `maxLatencyMs: 0`, …)
 * so a case with no positive assertion isn't treated as one that must contain
 * nothing. Throws a `ZodError` on a structurally invalid document.
 */
export function parseCases(json: string): EvalCase[] {
  const raw: unknown = JSON.parse(json);
  const parsed = EvalCasesSchema.parse(raw);
  return parsed.map((c) => normalizeCase(c));
}

function normalizeCase(c: z.infer<typeof EvalCaseSchema>): EvalCase {
  const out: EvalCase = { name: c.name, input: c.input };
  if (c.expectContains && c.expectContains.length > 0) out.expectContains = c.expectContains;
  if (c.expectNotContains && c.expectNotContains.length > 0) {
    out.expectNotContains = c.expectNotContains;
  }
  if (c.expectRefusal) out.expectRefusal = true;
  if (c.maxLatencyMs && c.maxLatencyMs > 0) out.maxLatencyMs = c.maxLatencyMs;
  if (c.maxCostUsd && c.maxCostUsd !== '') out.maxCostUsd = c.maxCostUsd;
  return out;
}
