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

/**
 * ObjectMeta subset the typed client surfaces. Unknown server-set fields
 * (creationTimestamp, managedFields, …) are dropped by the strict parse — the
 * client exposes a curated view, not the raw metadata.
 */
export const ResourceMeta = z.object({
  name: z.string(),
  namespace: z.string().optional(),
  uid: z.string().optional(),
  resourceVersion: z.string().optional(),
  labels: z.record(z.string(), z.string()).optional(),
  annotations: z.record(z.string(), z.string()).optional(),
});
export type ResourceMeta = z.infer<typeof ResourceMeta>;

/**
 * Full-object schemas for the CRs the client reads back from the cluster.
 * These are the read-boundary contract: the client parses raw API responses
 * through them rather than type-asserting, so a drifted or truncated response
 * surfaces as a validation error instead of an unchecked cast.
 */
export const PlatformResource = z.object({
  apiVersion: z.literal('platform.nanohype.dev/v1alpha1'),
  kind: z.literal('Platform'),
  metadata: ResourceMeta,
  spec: PlatformSpec,
  status: z
    .object({
      phase: z.string().optional(),
      iamRoleArn: z.string().optional(),
      namespace: z.string().optional(),
    })
    .optional(),
});
export type PlatformResource = z.infer<typeof PlatformResource>;

export const ModelGatewayResource = z.object({
  apiVersion: z.literal('agents.nanohype.dev/v1alpha1'),
  kind: z.literal('ModelGateway'),
  metadata: ResourceMeta,
  spec: z.object({
    platformRef: z.object({ name: z.string() }),
    routes: z.array(ModelRouteSpec),
    defaultGuardrailRef: z.object({ name: z.string() }).optional(),
  }),
  status: z.object({ phase: z.string().optional(), endpoint: z.string().optional() }).optional(),
});
export type ModelGatewayResource = z.infer<typeof ModelGatewayResource>;

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
  /**
   * True when modelId had no pricing entry, so costUsd is an unmetered 0
   * rather than a real $0. Lets cost dashboards surface unpriced traffic
   * instead of silently undercounting spend.
   */
  unpriced: z.boolean().optional(),
  latencyMs: z.number().nonnegative(),
  status: z.enum(['ok', 'error']),
  errorClass: z.string().optional(),
  timestamp: z.string().datetime(),
});
export type CallEvent = z.infer<typeof CallEvent>;
