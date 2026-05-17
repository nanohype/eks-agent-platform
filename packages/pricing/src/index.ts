import type { TokenUsage } from '@eks-agent/core';

/**
 * Bedrock on-demand prices per million tokens.
 *
 * MAINTENANCE: keep this file Renovate-managed (or refreshed weekly via the
 * pricing scrape script in scripts/refresh-pricing.mjs). Never hand-edit
 * values from memory — Bedrock pricing changes regularly and outdated
 * numbers silently undercount spend.
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

export function estimateCost({ modelId, tokens }: { modelId: string; tokens: TokenUsage }): number {
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
  if (!price) return 0;
  const input = (tokens.inputTokens / 1_000_000) * price.inputPerMillion;
  const output = (tokens.outputTokens / 1_000_000) * price.outputPerMillion;
  const cacheRead =
    price.cacheReadPerMillion !== undefined ? (tokens.cacheReadTokens / 1_000_000) * price.cacheReadPerMillion : 0;
  const cacheWrite =
    price.cacheWritePerMillion !== undefined ? (tokens.cacheWriteTokens / 1_000_000) * price.cacheWritePerMillion : 0;
  return input + output + cacheRead + cacheWrite;
}
