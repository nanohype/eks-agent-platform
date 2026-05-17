import { z } from 'zod';

/**
 * Schemas mirroring the operator's CRD types. These are the *runtime*
 * validation contract for any code that reads CRs back out of the cluster.
 */

export const ComplianceSpec = z.object({
  hipaa: z.boolean().default(false),
  soc2: z.boolean().default(false),
});
export type ComplianceSpec = z.infer<typeof ComplianceSpec>;

export const IdentitySpec = z.object({
  allowedModels: z.array(z.string()).default([]),
  allowedModelFamilies: z.array(z.string()).default([]),
  extraPolicyArns: z.array(z.string()).default([]),
});
export type IdentitySpec = z.infer<typeof IdentitySpec>;

export const PlatformPersona = z.enum([
  'sales-ops',
  'support',
  'finance',
  'ops',
  'founder',
  'eng',
  'marketing',
  'legal',
  'generic',
]);
export type PlatformPersona = z.infer<typeof PlatformPersona>;

export const PlatformSpec = z.object({
  displayName: z.string().optional(),
  persona: PlatformPersona,
  tenant: z.string(),
  budget: z.object({ name: z.string() }),
  identity: IdentitySpec,
  compliance: ComplianceSpec.optional(),
  isolation: z.enum(['namespace', 'vcluster']).default('namespace'),
});
export type PlatformSpec = z.infer<typeof PlatformSpec>;

export const ModelFamily = z.enum([
  'anthropic',
  'meta',
  'mistral',
  'cohere',
  'amazon-titan',
  'amazon-nova',
  'stability',
]);
export type ModelFamily = z.infer<typeof ModelFamily>;

export const ModelRouteSpec = z.object({
  name: z.string(),
  modelFamily: ModelFamily,
  modelId: z.string(),
  crossRegionProfile: z.string().optional(),
  rateLimit: z.number().int().positive().optional(),
  guardrailRef: z.object({ name: z.string() }).optional(),
});
export type ModelRouteSpec = z.infer<typeof ModelRouteSpec>;

export const TokenUsage = z.object({
  inputTokens: z.number().int().nonnegative(),
  outputTokens: z.number().int().nonnegative(),
  cacheReadTokens: z.number().int().nonnegative().default(0),
  cacheWriteTokens: z.number().int().nonnegative().default(0),
});
export type TokenUsage = z.infer<typeof TokenUsage>;

export const CallEvent = z.object({
  correlationId: z.string(),
  platform: z.string(),
  tenant: z.string(),
  workspace: z.string().optional(),
  modelFamily: ModelFamily,
  modelId: z.string(),
  tokens: TokenUsage,
  costUsd: z.number().nonnegative(),
  latencyMs: z.number().nonnegative(),
  status: z.enum(['ok', 'error']),
  errorClass: z.string().optional(),
  timestamp: z.string().datetime(),
});
export type CallEvent = z.infer<typeof CallEvent>;
