import type { TokenUsage } from '@eks-agent/core';

/**
 * Bedrock on-demand prices per million tokens.
 *
 * MAINTENANCE: this table is hand-curated and is NOT Renovate-managed
 * (Renovate bumps package deps, not PRICES content). An automated refresh from
 * the AWS Pricing API (scripts/refresh-pricing.mjs) is Phase-2 and currently a
 * fail-loud scaffold. Until then, update values by hand from the Bedrock
 * pricing page — never from memory, since Bedrock pricing changes regularly. A
 * model id missing from this table prices as an unmetered 0 (priced:false via
 * priceModel), so add new models here before they bill.
 *
 * Prices are USD per 1,000,000 tokens.
 */
export interface ModelPrice {
  inputPerMillion: number;
  outputPerMillion: number;
  cacheReadPerMillion?: number;
  cacheWritePerMillion?: number;
}

export const PRICES: Record<string, ModelPrice> = {
  // Anthropic Claude (Bedrock)
  'anthropic.claude-3-5-sonnet-20241022-v2:0': {
    inputPerMillion: 3.0,
    outputPerMillion: 15.0,
    cacheReadPerMillion: 0.3,
    cacheWritePerMillion: 3.75,
  },
  'us.anthropic.claude-3-5-sonnet-20241022-v2:0': {
    inputPerMillion: 3.0,
    outputPerMillion: 15.0,
    cacheReadPerMillion: 0.3,
    cacheWritePerMillion: 3.75,
  },
  'anthropic.claude-3-5-haiku-20241022-v1:0': {
    inputPerMillion: 0.8,
    outputPerMillion: 4.0,
    cacheReadPerMillion: 0.08,
    cacheWritePerMillion: 1.0,
  },
  'anthropic.claude-3-opus-20240229-v1:0': { inputPerMillion: 15.0, outputPerMillion: 75.0 },

  // Amazon Nova
  'amazon.nova-pro-v1:0': { inputPerMillion: 0.8, outputPerMillion: 3.2 },
  'amazon.nova-lite-v1:0': { inputPerMillion: 0.06, outputPerMillion: 0.24 },
  'amazon.nova-micro-v1:0': { inputPerMillion: 0.035, outputPerMillion: 0.14 },

  // Meta Llama 3 (representative)
  'meta.llama3-1-70b-instruct-v1:0': { inputPerMillion: 0.99, outputPerMillion: 0.99 },
  'meta.llama3-1-8b-instruct-v1:0': { inputPerMillion: 0.22, outputPerMillion: 0.22 },

  // Mistral
  'mistral.mistral-large-2407-v1:0': { inputPerMillion: 2.0, outputPerMillion: 6.0 },

  // Cohere
  'cohere.command-r-plus-v1:0': { inputPerMillion: 3.0, outputPerMillion: 15.0 },
  'cohere.command-r-v1:0': { inputPerMillion: 0.5, outputPerMillion: 1.5 },
};

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
  // Bedrock cross-region inference profile prefixes: us. eu. apac. ap. (and
  // future regional shorts). Strip any 2–5-char lowercase prefix before lookup
  // so profile IDs resolve to the base model price.
  const key = modelId.replace(/^[a-z]{2,5}\./, '');
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
