/**
 * Branded ID types. The brand is erased at runtime but enforced at compile time —
 * passing a TenantId where a PlatformId is expected is a type error.
 */

declare const brand: unique symbol;
type Brand<T, B extends string> = T & { readonly [brand]: B };

export type TenantId = Brand<string, 'TenantId'>;
export type PlatformId = Brand<string, 'PlatformId'>;
export type WorkspaceId = Brand<string, 'WorkspaceId'>;
export type AgentId = Brand<string, 'AgentId'>;
export type ModelRouteName = Brand<string, 'ModelRouteName'>;
export type CorrelationId = Brand<string, 'CorrelationId'>;

// Kubernetes resource-name max is 63 chars (RFC 1123 subdomain label).
// Min 3 keeps slugs readable. Branded IDs feed namespace + CR names that
// must pass kube-apiserver validation, so the regex max is 63 here too.
const slugRe = /^[a-z][a-z0-9-]{1,61}[a-z0-9]$/;

function brandSlug<T extends Brand<string, string>>(kind: string, raw: string): T {
  if (!slugRe.test(raw)) {
    throw new Error(
      `invalid ${kind}: must be 3-63 chars, lowercase, alphanumeric + hyphens, start with letter, end with letter or digit`,
    );
  }
  return raw as T;
}

export const TenantId = (s: string): TenantId => brandSlug<TenantId>('TenantId', s);
export const PlatformId = (s: string): PlatformId => brandSlug<PlatformId>('PlatformId', s);
export const WorkspaceId = (s: string): WorkspaceId => brandSlug<WorkspaceId>('WorkspaceId', s);
export const AgentId = (s: string): AgentId => brandSlug<AgentId>('AgentId', s);
export const ModelRouteName = (s: string): ModelRouteName =>
  brandSlug<ModelRouteName>('ModelRouteName', s);

export const CorrelationId = (): CorrelationId => crypto.randomUUID() as unknown as CorrelationId;
