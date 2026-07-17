import {
  ModelGatewayResource,
  PlatformResource,
  type ModelGatewayResource as ModelGateway,
  type PlatformResource as Platform,
} from '@eks-agent/core';
import { KubeConfig, CustomObjectsApi } from '@kubernetes/client-node';

export type { ResourceMeta } from '@eks-agent/core';
export type { ModelGateway, Platform };

// The operator's CRDs are split across three capability groups under the
// nanohype.dev domain. Each kind maps to the group that owns it.
const GROUPS = {
  platform: 'platform.nanohype.dev',
  agents: 'agents.nanohype.dev',
  governance: 'governance.nanohype.dev',
} as const;
const VERSION = 'v1alpha1';

/** Default per-call deadline for CRD operations (ms). */
const DEFAULT_TIMEOUT_MS = 30_000;

/** Page size for list pagination; the server may return fewer. */
const LIST_PAGE_SIZE = 100;

/**
 * The slice of CustomObjectsApi the client actually calls. Injectable via
 * ClientOptions.api so tests and embedders supply a fake at the client
 * seam instead of module-mocking the kubernetes SDK.
 */
export type CustomObjectsClient = Pick<
  CustomObjectsApi,
  | 'listClusterCustomObject'
  | 'getClusterCustomObject'
  | 'createClusterCustomObject'
  | 'deleteClusterCustomObject'
  | 'listNamespacedCustomObject'
>;

export interface ClientOptions {
  /** Optional explicit kubeconfig path; defaults to KUBECONFIG env or in-cluster config. */
  kubeconfigPath?: string;
  context?: string;
  /** Pre-built API client; when set, kubeconfig resolution is skipped entirely. */
  api?: CustomObjectsClient;
  /** Per-call deadline in ms applied to every CRD operation. Defaults to 30s. */
  timeoutMs?: number;
}

/** Per-call options — a caller AbortSignal composed with the client deadline. */
export interface CallOptions {
  /** Caller cancellation, combined with the client's default deadline (earliest fire wins). */
  signal?: AbortSignal;
}

/**
 * The subset of `@kubernetes/client-node`'s KubeConfig the client drives to
 * resolve an API client. Declaring it lets tests exercise the resolution
 * precedence (explicit path → KUBECONFIG → in-cluster → default) with a fake
 * KubeConfig, instead of leaving the constructor's branch uncovered because it
 * needs a real kubeconfig file or in-cluster env.
 */
export type KubeConfigLoader = Pick<
  KubeConfig,
  'loadFromFile' | 'loadFromCluster' | 'loadFromDefault' | 'setCurrentContext' | 'makeApiClient'
>;

/**
 * Resolves the CustomObjects API client from a kubeconfig, honoring the
 * precedence explicit path → `KUBECONFIG` env → in-cluster → default, then an
 * optional context override. The KubeConfig is injectable so the resolution
 * branches are unit-tested hermetically; production passes a fresh KubeConfig.
 */
export function resolveApi(
  opts: ClientOptions,
  kc: KubeConfigLoader = new KubeConfig(),
): CustomObjectsClient {
  if (opts.kubeconfigPath) {
    kc.loadFromFile(opts.kubeconfigPath);
  } else if (process.env.KUBECONFIG) {
    kc.loadFromFile(process.env.KUBECONFIG);
  } else if (process.env.KUBERNETES_SERVICE_HOST) {
    kc.loadFromCluster();
  } else {
    kc.loadFromDefault();
  }
  if (opts.context) kc.setCurrentContext(opts.context);
  return kc.makeApiClient(CustomObjectsApi);
}

/**
 * Compose a bounded deadline for a call: the client's default request timeout is
 * always applied, and a caller-supplied AbortSignal is combined with it (earliest
 * fire wins). Mirrors the Bedrock adapter's timeout idiom so both layers cancel
 * the same way.
 */
export function deadlineSignal(timeoutMs: number, caller?: AbortSignal): AbortSignal {
  const deadline = AbortSignal.timeout(timeoutMs);
  return caller ? AbortSignal.any([caller, deadline]) : deadline;
}

/**
 * Normalize an AbortSignal's reason to a throwable Error. `AbortSignal.timeout`
 * yields a TimeoutError; a caller may pass any reason.
 */
function abortReason(signal: AbortSignal): Error {
  const reason: unknown = signal.reason;
  return reason instanceof Error ? reason : new Error('aborted', { cause: reason });
}

/**
 * Settle with the operation, or reject as soon as `signal` fires — whichever
 * comes first. The `@kubernetes/client-node` CustomObjects methods take no
 * per-call signal, so racing bounds the caller-visible latency: a hung API
 * server can't stall a reconcile loop past the deadline, and a caller abort
 * resolves promptly. `op`'s own rejection propagates unchanged, and it keeps a
 * handler attached via the race so it never surfaces as an unhandled rejection.
 */
function abortable<T>(op: Promise<T>, signal: AbortSignal): Promise<T> {
  // Already aborted (e.g. a caller passed a fired signal): reject deterministically
  // rather than racing, so a fast-resolving op can't win over the abort.
  if (signal.aborted) return Promise.reject(abortReason(signal));
  const aborted = new Promise<never>((_resolve, reject) => {
    signal.addEventListener('abort', () => reject(abortReason(signal)), { once: true });
  });
  return Promise.race([op, aborted]);
}

/** The list envelope shape: items plus the pagination continue token. */
interface ListEnvelope {
  items?: unknown[];
  metadata?: { continue?: string };
}

export class EksAgentClient {
  readonly api: CustomObjectsClient;
  private readonly timeoutMs: number;

  constructor(opts: ClientOptions = {}) {
    this.api = opts.api ?? resolveApi(opts);
    this.timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  }

  /**
   * Walk every page of a list call (following `metadata.continue`), parsing each
   * raw item through its resource schema at the read boundary. The whole walk
   * shares one deadline so pagination can't extend past the caller's timeout.
   */
  private async listAll<T>(
    fetchPage: (cont: string | undefined) => Promise<unknown>,
    parse: (raw: unknown) => T,
    signal: AbortSignal,
  ): Promise<T[]> {
    const items: T[] = [];
    let cont: string | undefined;
    do {
      const page = (await abortable(fetchPage(cont), signal)) as ListEnvelope;
      for (const raw of page.items ?? []) items.push(parse(raw));
      cont = page.metadata?.continue;
    } while (cont);
    return items;
  }

  async listPlatforms(opts: CallOptions = {}): Promise<Platform[]> {
    const signal = deadlineSignal(this.timeoutMs, opts.signal);
    return this.listAll(
      (cont) =>
        this.api.listClusterCustomObject({
          group: GROUPS.platform,
          version: VERSION,
          plural: 'platforms',
          limit: LIST_PAGE_SIZE,
          ...(cont ? { _continue: cont } : {}),
        }),
      (raw) => PlatformResource.parse(raw),
      signal,
    );
  }

  async getPlatform(name: string, opts: CallOptions = {}): Promise<Platform> {
    const signal = deadlineSignal(this.timeoutMs, opts.signal);
    const r: unknown = await abortable(
      this.api.getClusterCustomObject({
        group: GROUPS.platform,
        version: VERSION,
        plural: 'platforms',
        name,
      }),
      signal,
    );
    return PlatformResource.parse(r);
  }

  async applyPlatform(p: Platform, opts: CallOptions = {}): Promise<Platform> {
    const signal = deadlineSignal(this.timeoutMs, opts.signal);
    const r: unknown = await abortable(
      this.api.createClusterCustomObject({
        group: GROUPS.platform,
        version: VERSION,
        plural: 'platforms',
        body: p,
      }),
      signal,
    );
    return PlatformResource.parse(r);
  }

  async deletePlatform(name: string, opts: CallOptions = {}): Promise<void> {
    const signal = deadlineSignal(this.timeoutMs, opts.signal);
    await abortable(
      this.api.deleteClusterCustomObject({
        group: GROUPS.platform,
        version: VERSION,
        plural: 'platforms',
        name,
      }),
      signal,
    );
  }

  async listModelGateways(namespace: string, opts: CallOptions = {}): Promise<ModelGateway[]> {
    const signal = deadlineSignal(this.timeoutMs, opts.signal);
    return this.listAll(
      (cont) =>
        this.api.listNamespacedCustomObject({
          group: GROUPS.agents,
          version: VERSION,
          namespace,
          plural: 'modelgateways',
          limit: LIST_PAGE_SIZE,
          ...(cont ? { _continue: cont } : {}),
        }),
      (raw) => ModelGatewayResource.parse(raw),
      signal,
    );
  }
}
