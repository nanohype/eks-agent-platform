import { z } from 'zod';

/**
 * Schemas mirroring the operator's CRD types. These are the *runtime*
 * validation contract for any code that reads CRs back out of the cluster.
 *
 * Field names and shapes track the Go API types under
 * `operators/api/**` one-for-one. The drift gate
 * (`scripts/check-schema-drift.mts`, wired into CI) diffs these zod shapes
 * against the generated CRD OpenAPI schemas and fails the build if a spec or
 * status field is present on one side but not the other — so the typed client
 * can never silently go blind to a field the operator writes (the way it once
 * was for `spec.attribution` and the whole suspension-state status block).
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

/**
 * AttributionSpec opts a Platform into per-session human attribution. Mirrors
 * the Go `AttributionSpec` — `operators` is the set of human identities a
 * session may act as (min 1); `sessionRoleMaxDurationSeconds` caps the assumed
 * session lifetime (defaulted server-side to 3600).
 */
export const AttributionSpec = z.object({
  operators: z.array(z.string()).min(1),
  sessionRoleMaxDurationSeconds: z.number().int().min(900).max(43200).default(3600),
});
export type AttributionSpec = z.infer<typeof AttributionSpec>;

/**
 * Condition mirrors k8s `metav1.Condition` — the standard status-condition
 * shape the operator writes onto Platform/ModelGateway status.
 */
export const Condition = z.object({
  type: z.string(),
  status: z.enum(['True', 'False', 'Unknown']),
  observedGeneration: z.number().int().optional(),
  lastTransitionTime: z.string().datetime(),
  reason: z.string(),
  message: z.string(),
});
export type Condition = z.infer<typeof Condition>;

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
  attribution: AttributionSpec.optional(),
});
export type PlatformSpec = z.infer<typeof PlatformSpec>;

/**
 * PlatformStatus mirrors the Go `PlatformStatus`. Every field the operator
 * writes is modelled so a client reading a Platform back sees the full picture
 * — including the suspension-state block (`suspendedAt`/`suspendedReason`) the
 * kill-switch sets, which callers must be able to observe.
 */
export const PlatformStatus = z.object({
  phase: z.string().optional(),
  iamRoleArn: z.string().optional(),
  sessionRoleArn: z.string().optional(),
  namespace: z.string().optional(),
  observedGeneration: z.number().int().optional(),
  suspendedAt: z.string().datetime().optional(),
  suspendedReason: z.string().optional(),
  conditions: z.array(Condition).optional(),
});
export type PlatformStatus = z.infer<typeof PlatformStatus>;

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
  status: PlatformStatus.optional(),
});
export type PlatformResource = z.infer<typeof PlatformResource>;

/** ModelGatewaySpec mirrors the Go `ModelGatewaySpec`. */
export const ModelGatewaySpec = z.object({
  platformRef: z.object({ name: z.string() }),
  routes: z.array(ModelRouteSpec),
  defaultGuardrailRef: z.object({ name: z.string() }).optional(),
});
export type ModelGatewaySpec = z.infer<typeof ModelGatewaySpec>;

/** ModelGatewayStatus mirrors the Go `ModelGatewayStatus`. */
export const ModelGatewayStatus = z.object({
  phase: z.string().optional(),
  endpoint: z.string().optional(),
  observedGeneration: z.number().int().optional(),
  conditions: z.array(Condition).optional(),
});
export type ModelGatewayStatus = z.infer<typeof ModelGatewayStatus>;

export const ModelGatewayResource = z.object({
  apiVersion: z.literal('agents.nanohype.dev/v1alpha1'),
  kind: z.literal('ModelGateway'),
  metadata: ResourceMeta,
  spec: ModelGatewaySpec,
  status: ModelGatewayStatus.optional(),
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
