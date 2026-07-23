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
  capabilities: z.array(z.enum(['ses', 'eventBridgeScheduler'])).default([]),
  directSecretReads: z.array(z.string()).default([]),
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

/**
 * Datastore schemas mirror the Go datastore vocabulary
 * (`operators/api/platform/v1alpha1/datastore_types.go`). A Platform declares
 * the stateful stores it needs; the kind selects an AWS implementation and, at
 * most, the one config block matching it (stream carries none). Field names and
 * shapes track the Go types one-for-one so the drift gate stays satisfied and
 * the typed client never goes blind to a declared datastore.
 */
export const DatastoreKind = z.enum([
  'relational',
  'keyValue',
  'objectStore',
  'queue',
  'cache',
  'stream',
]);
export type DatastoreKind = z.infer<typeof DatastoreKind>;

/** AttributeSchema mirrors the Go `AttributeSchema` — a DynamoDB key attribute. */
export const AttributeSchema = z.object({
  name: z.string(),
  type: z.enum(['S', 'N', 'B']),
});
export type AttributeSchema = z.infer<typeof AttributeSchema>;

/** GlobalSecondaryIndex mirrors the Go `GlobalSecondaryIndex` — a DynamoDB GSI. */
export const GlobalSecondaryIndex = z.object({
  name: z.string(),
  partitionKey: AttributeSchema,
  sortKey: AttributeSchema.optional(),
  projection: z.enum(['ALL', 'KEYS_ONLY', 'INCLUDE']).default('ALL'),
});
export type GlobalSecondaryIndex = z.infer<typeof GlobalSecondaryIndex>;

/** RelationalConfig mirrors the Go `RelationalConfig` — Aurora Serverless v2. */
export const RelationalConfig = z.object({
  engineVersion: z.string().default('16.6'),
  // ACU is fractional; serialized as a string, matching the Go/CRD side.
  minACU: z.string().default('0.5'),
  maxACU: z.string().default('8'),
  backupRetentionDays: z.number().int().min(1).max(35).default(7),
  deletionProtection: z.boolean().default(true),
});
export type RelationalConfig = z.infer<typeof RelationalConfig>;

/** KeyValueConfig mirrors the Go `KeyValueConfig` — a DynamoDB table. */
export const KeyValueConfig = z.object({
  partitionKey: AttributeSchema,
  sortKey: AttributeSchema.optional(),
  billingMode: z.enum(['PAY_PER_REQUEST', 'PROVISIONED']).default('PAY_PER_REQUEST'),
  ttlAttribute: z.string().optional(),
  pointInTimeRecovery: z.boolean().default(true),
  globalSecondaryIndexes: z.array(GlobalSecondaryIndex).optional(),
});
export type KeyValueConfig = z.infer<typeof KeyValueConfig>;

/** ObjectStoreConfig mirrors the Go `ObjectStoreConfig` — an S3 bucket. */
export const ObjectStoreConfig = z.object({
  versioning: z.boolean().default(true),
  lifecycleExpireDays: z.number().int().min(0).default(0),
});
export type ObjectStoreConfig = z.infer<typeof ObjectStoreConfig>;

/** QueueConfig mirrors the Go `QueueConfig` — an SQS queue. */
export const QueueConfig = z.object({
  fifo: z.boolean().default(false),
  visibilityTimeoutSeconds: z.number().int().min(0).max(43200).default(30),
  messageRetentionSeconds: z.number().int().min(60).max(1209600).default(345600),
  maxReceiveCount: z.number().int().min(0).max(1000).default(0),
});
export type QueueConfig = z.infer<typeof QueueConfig>;

/** CacheConfig mirrors the Go `CacheConfig` — an ElastiCache cluster. */
export const CacheConfig = z.object({
  engine: z.enum(['valkey', 'redis']).default('valkey'),
  nodeType: z.string().default('cache.t4g.micro'),
  replicas: z.number().int().min(0).max(5).default(0),
});
export type CacheConfig = z.infer<typeof CacheConfig>;

/**
 * DatastoreSpec mirrors the Go `DatastoreSpec` — one declared stateful store.
 * The kind selects the AWS implementation and, at most, the one config block
 * matching it; the CRD enforces the kind↔block relation at admission.
 */
export const DatastoreSpec = z.object({
  name: z.string(),
  kind: DatastoreKind,
  deletionPolicy: z.enum(['Retain', 'Delete']).default('Retain'),
  relational: RelationalConfig.optional(),
  keyValue: KeyValueConfig.optional(),
  objectStore: ObjectStoreConfig.optional(),
  queue: QueueConfig.optional(),
  cache: CacheConfig.optional(),
});
export type DatastoreSpec = z.infer<typeof DatastoreSpec>;

/**
 * DatastoreStatus mirrors the Go `DatastoreStatus` — one datastore's observed
 * state, reported separately from the top-level phase.
 */
export const DatastoreStatus = z.object({
  name: z.string(),
  kind: DatastoreKind.optional(),
  phase: z.string().optional(),
  endpoint: z.string().optional(),
  arn: z.string().optional(),
  secretName: z.string().optional(),
  drift: z.array(z.string()).optional(),
});
export type DatastoreStatus = z.infer<typeof DatastoreStatus>;

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
  datastores: z.array(DatastoreSpec).optional(),
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
  datastores: z.array(DatastoreStatus).optional(),
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
