import type { TokenUsage } from '@eks-agent/core';

import bedrockPricing from './data/bedrock-pricing.json' with { type: 'json' };

/**
 * Bedrock on-demand prices per million tokens.
 *
 * The price table is the single source of truth in
 * `src/data/bedrock-pricing.json`. This module imports it directly; the
 * Lambda cost publisher's Python table is generated from the same file
 * (`scripts/gen-lambda-pricing.mjs`) and a CI drift gate fails the build if
 * the two diverge. Refresh the JSON from the AWS Pricing API with
 * `scripts/refresh-pricing.mjs` (documented weekly cadence). A model id
 * missing from the table prices as an unmetered 0 (priced:false via
 * {@link priceModel}), so add new models to the JSON before they bill.
 *
 * Prices are USD per 1,000,000 tokens.
 */
export interface ModelPrice {
  inputPerMillion: number;
  outputPerMillion: number;
  cacheReadPerMillion?: number;
  cacheWritePerMillion?: number;
}

interface PricingEntry extends ModelPrice {
  family: string;
}

const PRICING_DATA = bedrockPricing.models as Record<string, PricingEntry>;

export const PRICES: Record<string, ModelPrice> = Object.fromEntries(
  Object.entries(PRICING_DATA).map(([id, { family: _family, ...price }]) => [id, price]),
);

/**
 * Strip a Bedrock cross-region inference-profile prefix (`us.`, `eu.`,
 * `jp.`, `ap.`, `apac.`, `global.`, …) from a model id, leaving the bare
 * `<provider>.<model>` id. Removes exactly one leading lowercase segment and
 * only when a `<provider>.<model>` id remains (i.e. the remainder still
 * contains a dot), so a bare provider id like
 * `anthropic.claude-3-opus-20240229-v1:0` is left untouched. Pattern-based —
 * no hardcoded geo list — so future regional shorts resolve automatically.
 */
export function bareModel(modelId: string): string {
  const dot = modelId.indexOf('.');
  if (dot > 0 && /^[a-z]+$/.test(modelId.slice(0, dot)) && modelId.slice(dot + 1).includes('.')) {
    return modelId.slice(dot + 1);
  }
  return modelId;
}

export interface PriceResult {
  /** Estimated USD cost. 0 when the model id has no entry in PRICES. */
  costUsd: number;
  /**
   * False when modelId had no PRICES entry — costUsd is then an unmetered 0,
   * not a real $0. Lets callers flag unpriced traffic rather than silently
   * undercounting spend on a new or mistyped model id.
   */
  priced: boolean;
}

/**
 * Price a call, surfacing whether the model id was actually found. Prefer this
 * over estimateCost on metering paths so an unpriced model is observable
 * instead of silently zeroed.
 */
export function priceModel({
  modelId,
  tokens,
}: {
  modelId: string;
  tokens: TokenUsage;
}): PriceResult {
  const key = bareModel(modelId);
  // PRICES is a static, hand-curated record. Bracket lookup is safe — no
  // prototype-pollution surface, no method invocation, no user-controlled
  // writes. The security/detect-object-injection rule is a false positive
  // for static read-only catalogs.
  // eslint-disable-next-line security/detect-object-injection
  const price = PRICES[modelId] ?? PRICES[key];
  if (!price) return { costUsd: 0, priced: false };
  const input = (tokens.inputTokens / 1_000_000) * price.inputPerMillion;
  const output = (tokens.outputTokens / 1_000_000) * price.outputPerMillion;
  const cacheRead =
    price.cacheReadPerMillion !== undefined
      ? (tokens.cacheReadTokens / 1_000_000) * price.cacheReadPerMillion
      : 0;
  const cacheWrite =
    price.cacheWritePerMillion !== undefined
      ? (tokens.cacheWriteTokens / 1_000_000) * price.cacheWritePerMillion
      : 0;
  return { costUsd: input + output + cacheRead + cacheWrite, priced: true };
}

/**
 * Estimate USD cost for a call. Returns 0 for an unknown model id — use
 * {@link priceModel} when you need to distinguish a real $0 from an
 * unmetered miss.
 */
export function estimateCost(args: { modelId: string; tokens: TokenUsage }): number {
  return priceModel(args).costUsd;
}
