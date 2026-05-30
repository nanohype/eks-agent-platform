# @eks-agent/client

Typed Kubernetes client for the `{platform,agents,governance}.nanohype.dev` CRDs. Wraps `@kubernetes/client-node` with explicit types pulled from `@eks-agent/core` schemas.

```ts
import { EksAgentClient } from '@eks-agent/client';

const client = new EksAgentClient();
const platforms = await client.listPlatforms();
for (const p of platforms) {
  console.log(p.metadata.name, p.spec.persona, p.status?.phase);
}
```

A future improvement will expose `kubebuilder`-generated client interfaces produced from the CRD OpenAPI v3 schemas (run `task client:gen` once the script is wired). Until then, the hand-written types above are the source of truth.
