import type { PlatformSpec, ModelRouteSpec } from '@eks-agent/core';
import { KubeConfig, CustomObjectsApi } from '@kubernetes/client-node';

// The operator's CRDs are split across three capability groups under the
// nanohype.dev domain. Each kind maps to the group that owns it.
const GROUPS = {
  platform: 'platform.nanohype.dev',
  agents: 'agents.nanohype.dev',
  governance: 'governance.nanohype.dev',
} as const;
const VERSION = 'v1alpha1';

export interface ResourceMeta {
  name: string;
  namespace?: string;
  uid?: string;
  resourceVersion?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
}

export interface Platform {
  apiVersion: `${typeof GROUPS.platform}/${typeof VERSION}`;
  kind: 'Platform';
  metadata: ResourceMeta;
  spec: PlatformSpec;
  status?: { phase?: string; iamRoleArn?: string; namespace?: string };
}

export interface ModelGateway {
  apiVersion: `${typeof GROUPS.agents}/${typeof VERSION}`;
  kind: 'ModelGateway';
  metadata: ResourceMeta;
  spec: {
    platformRef: { name: string };
    routes: ModelRouteSpec[];
    defaultGuardrailRef?: { name: string };
  };
  status?: { phase?: string; endpoint?: string };
}

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
}

export class EksAgentClient {
  readonly api: CustomObjectsClient;

  constructor(opts: ClientOptions = {}) {
    if (opts.api) {
      this.api = opts.api;
      return;
    }
    const kc = new KubeConfig();
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
    this.api = kc.makeApiClient(CustomObjectsApi);
  }

  async listPlatforms(): Promise<Platform[]> {
    const r: unknown = await this.api.listClusterCustomObject({
      group: GROUPS.platform,
      version: VERSION,
      plural: 'platforms',
    });
    return (r as { items?: Platform[] }).items ?? [];
  }

  async getPlatform(name: string): Promise<Platform> {
    const r: unknown = await this.api.getClusterCustomObject({
      group: GROUPS.platform,
      version: VERSION,
      plural: 'platforms',
      name,
    });
    return r as Platform;
  }

  async applyPlatform(p: Platform): Promise<Platform> {
    const r: unknown = await this.api.createClusterCustomObject({
      group: GROUPS.platform,
      version: VERSION,
      plural: 'platforms',
      body: p,
    });
    return r as Platform;
  }

  async deletePlatform(name: string): Promise<void> {
    await this.api.deleteClusterCustomObject({
      group: GROUPS.platform,
      version: VERSION,
      plural: 'platforms',
      name,
    });
  }

  async listModelGateways(namespace: string): Promise<ModelGateway[]> {
    const r: unknown = await this.api.listNamespacedCustomObject({
      group: GROUPS.agents,
      version: VERSION,
      namespace,
      plural: 'modelgateways',
    });
    return (r as { items?: ModelGateway[] }).items ?? [];
  }
}
