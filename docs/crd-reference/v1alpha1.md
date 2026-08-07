# API Reference

## Packages
- [agents.nanohype.dev/v1alpha1](#agentsnanohypedevv1alpha1)
- [governance.nanohype.dev/v1alpha1](#governancenanohypedevv1alpha1)
- [platform.nanohype.dev/v1alpha1](#platformnanohypedevv1alpha1)


## agents.nanohype.dev/v1alpha1

Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group.

### Resource Types
- [AgentFleet](#agentfleet)
- [AgentSandbox](#agentsandbox)
- [ModelGateway](#modelgateway)
- [SandboxPool](#sandboxpool)



#### AgentFleet



AgentFleet is a Platform-scoped declaration of one or more agents, each
reconciled into a Deployment running under the tenant's identity. The scale
subresource is deliberately omitted: `kubectl scale` would be ambiguous
(min? max? per-agent?) for a fleet,
so per-agent replica overrides live on AgentSpec.Replicas and fleet-wide
behavior is driven by .spec.scaling (KEDA) instead.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `AgentFleet` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[AgentFleetSpec](#agentfleetspec)_ |  |  |  |
| `status` _[AgentFleetStatus](#agentfleetstatus)_ |  |  |  |


#### AgentFleetSpec



AgentFleetSpec declares one or more agents and the platform scaffolding
around them (KEDA scaling, NetworkPolicy, the tenant identity binding).

Each agent runs as a Deployment in the tenant's namespace, under the tenant
ServiceAccount, executing the tenant's own image. The agent loop and its
tools live in that image and run in that process — so an action the agent
takes is taken *as the tenant*, and the Kubernetes audit log records the
tenant's identity against it. That is the property the platform exists to
provide: an agent's claim about what it did can be checked against the
record of what happened, because both name the same principal.



_Appears in:_
- [AgentFleet](#agentfleet)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ |  |  |  |
| `agents` _[AgentSpec](#agentspec) array_ | Agents is the list of agents to provision in this fleet. |  | MinItems: 1 <br /> |
| `scaling` _[ScalingSpec](#scalingspec)_ | Scaling controls KEDA's ScaledObject for the runtime Deployments. |  | Optional: \{\} <br /> |


#### AgentFleetStatus



AgentFleetStatus reports rollout state.



_Appears in:_
- [AgentFleet](#agentfleet)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Provisioning, Ready, ScaledToZero, Failed. |  | Optional: \{\} <br /> |
| `readyAgents` _integer_ | ReadyAgents counts agents whose downstream Deployment is ready. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | ObservedGeneration is the last spec.generation reconciled. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


#### AgentSandbox



AgentSandbox is a Platform-scoped, single-use isolated pod for one agent
role-session. It shares SandboxPool's hardening — Pod Security
"restricted", default-deny networked, on the dedicated tainted node pool —
but is push-dispatched (one session, run-once) rather than a pull-based
pool of always-on workers.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `AgentSandbox` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[AgentSandboxSpec](#agentsandboxspec)_ |  |  |  |
| `status` _[AgentSandboxStatus](#agentsandboxstatus)_ |  |  |  |


#### AgentSandboxSpec



AgentSandboxSpec declares one ephemeral, hardened pod that runs a single
agent role-session — fab's `sdk` role-loop dispatched per session. The
reconciler builds the pod on the dedicated, tainted sandbox node pool,
locked down by a default-deny NetworkPolicy, under the Platform's tenant
ServiceAccount — which carries the tenant's AWS identity through its EKS
Pod Identity association.



_Appears in:_
- [AgentSandbox](#agentsandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ | PlatformRef is the owning Platform. The session pod runs in that<br />Platform's tenant namespace and the sandbox gates on Platform<br />readiness. |  |  |
| `image` _string_ | Image is the container image the session pod runs. |  |  |
| `command` _string array_ | Command overrides the image entrypoint. |  | Optional: \{\} <br /> |
| `args` _string array_ | Args are the container arguments. |  | Optional: \{\} <br /> |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#envvar-v1-core) array_ | Env is the session pod's environment. The dispatcher (fab) passes the<br />role, the role message, and any backend config through here. |  | Optional: \{\} <br /> |
| `runtimeClassName` _string_ | RuntimeClassName selects a Kubernetes RuntimeClass for the session<br />pod — "gvisor" or "kata" for kernel-level isolation of the untrusted<br />agent code. The named RuntimeClass must already exist. Empty uses the<br />cluster's default runtime. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_ | Resources are the session pod's resource requests and limits. |  | Optional: \{\} <br /> |
| `ttlSecondsAfterFinished` _integer_ | TTLSecondsAfterFinished is how long the AgentSandbox is kept after its<br />session pod terminates before the operator garbage-collects it. | 3600 | Minimum: 0 <br />Optional: \{\} <br /> |


#### AgentSandboxStatus



AgentSandboxStatus reports the sandbox's reconciled state.



_Appears in:_
- [AgentSandbox](#agentsandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Running, Succeeded, Failed, Suspended. |  | Optional: \{\} <br /> |
| `podName` _string_ | PodName is the session pod's name in the tenant namespace. |  | Optional: \{\} <br /> |
| `podPhase` _string_ | PodPhase mirrors the session pod's status.phase. |  | Optional: \{\} <br /> |
| `completedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | CompletedAt is when the session pod first reached a terminal phase —<br />the start of the TTL countdown. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | ObservedGeneration is the last spec.generation reconciled. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


#### AgentSpec



AgentSpec is one agent in the fleet.



_Appears in:_
- [AgentFleetSpec](#agentfleetspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `systemPrompt` _string_ | SystemPrompt is the agent's instruction text. |  |  |
| `modelRoute` _string_ | ModelRoute is the named route on the Platform's ModelGateway. |  |  |
| `image` _string_ | Image is the container the agent runs — the tenant's own build, carrying<br />its agent loop and its tools.<br />There is no platform-supplied agent runtime and no separate tool server.<br />A tool server would execute the agent's actions under its own identity,<br />which is exactly what makes an action untraceable to the agent that<br />requested it: the audit log names the tool server, and the agent's claim<br />to have done something cannot be confirmed or refuted. Tools run in the<br />agent's process, as the tenant, so the two records line up. |  |  |
| `replicas` _integer_ | Replicas overrides the fleet-wide scaling minimum for this agent. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_ | Resources overrides the default container resources. The tenant's<br />ResourceQuota applies either way; this is for an agent that needs a<br />different shape than the default. |  | Optional: \{\} <br /> |


#### ModelGateway



ModelGateway is a per-Platform gateway CR that fronts Bedrock for one or more named routes.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `ModelGateway` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ModelGatewaySpec](#modelgatewayspec)_ |  |  |  |
| `status` _[ModelGatewayStatus](#modelgatewaystatus)_ |  |  |  |


#### ModelGatewaySpec



ModelGatewaySpec configures a per-Platform gateway: the routes exposed,
which Bedrock models back them, and which Guardrail attaches.



_Appears in:_
- [ModelGateway](#modelgateway)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ | PlatformRef is the owning Platform. |  |  |
| `routes` _[ModelRouteSpec](#modelroutespec) array_ | Routes is the list of named routes the gateway exposes. |  | MinItems: 1 <br /> |
| `defaultGuardrailRef` _[LocalRef](#localref)_ | DefaultGuardrailRef applies when a Route does not specify its own. |  | Optional: \{\} <br /> |


#### ModelGatewayStatus



ModelGatewayStatus surfaces the gateway's route and listener state.



_Appears in:_
- [ModelGateway](#modelgateway)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Provisioning, Ready, Failed. |  | Optional: \{\} <br /> |
| `endpoint` _string_ | Endpoint is the cluster-internal hostname of the gateway. It addresses<br />the gateway, not any one API on it — see Routes for the base URL a<br />client is actually configured with. |  | Optional: \{\} <br /> |
| `routes` _[RouteStatus](#routestatus) array_ | Routes is the per-route client contract: the resolved wire format and<br />the base URL that format is served at. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | ObservedGeneration is the last spec.generation reconciled. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


#### ModelRouteSpec



ModelRouteSpec is a single named route.



_Appears in:_
- [ModelGatewaySpec](#modelgatewayspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `modelSource` _[ModelSource](#modelsource)_ | ModelSource discriminates a foundation-model route from an imported<br />(Custom Model Import) route. Defaults to foundation, so an existing<br />route that omits it stays a foundation route. | foundation | Enum: [foundation imported] <br />Optional: \{\} <br /> |
| `modelFamily` _string_ | ModelFamily is the Bedrock model family for a foundation route:<br />anthropic \| meta \| mistral \| cohere \| amazon-titan \| amazon-nova \|<br />stability. Required for a foundation route, rejected for an imported one<br />(enforced by the route-level CEL rules above). |  | Enum: [anthropic meta mistral cohere amazon-titan amazon-nova stability] <br />Optional: \{\} <br /> |
| `modelId` _string_ | ModelID is the route's model. For a foundation route it is the canonical<br />Bedrock model ID or inference-profile ID; for an imported route it is the<br />imported-model ARN<br />(arn:<partition>:bedrock:<region>:<account>:imported-model/<id>). |  |  |
| `crossRegionProfile` _string_ | CrossRegionProfile enables a Bedrock cross-region inference profile.<br />Foundation routes only; rejected on an imported route. |  | Optional: \{\} <br /> |
| `api` _[RouteAPI](#routeapi)_ | API is the wire format callers speak to this route, and therefore which<br />base URL they must use — the gateway serves each format under its own<br />endpoint prefix. The reconciler publishes the resolved value and its base<br />URL on status.routes, so a caller reads the contract rather than assuming<br />it.<br />Left unset it is derived from the model: an anthropic-family foundation<br />route serves Anthropic, everything else serves OpenAI. There is no static<br />default, because one would be wrong for whichever kind of route it did<br />not describe — an embeddings route is not reachable as Anthropic, and<br />defaulting a Claude route to OpenAI would silently drop thinking blocks<br />and cache points.<br />Set it explicitly to pin the format across a model change: a route<br />declared OpenAI stays OpenAI when repointed from Claude to an<br />open-weight model, so the swap is a CR edit and the app is untouched. |  | Enum: [Anthropic OpenAI] <br />Optional: \{\} <br /> |
| `rateLimit` _integer_ | RateLimit caps requests per minute (not tokens) on this route. The<br />operator renders it into a local rate-limit rule on the gateway's<br />BackendTrafficPolicy; 0 or unset disables rate limiting for the route. |  | Optional: \{\} <br /> |
| `guardrailRef` _[LocalRef](#localref)_ | GuardrailRef overrides the gateway's default guardrail. On a foundation<br />route the guardrail attaches as request headers the caller cannot<br />override. On an imported<br />route an inline guardrail is not applicable (Bedrock inline guardrails are<br />foundation-model-only), so the route is served without one and the gateway<br />surfaces an ImportedRouteGuardrailUnenforced condition — enforcement via<br />ApplyGuardrail is a tracked follow-up. |  | Optional: \{\} <br /> |


#### ModelSource

_Underlying type:_ _string_

ModelSource discriminates how a route sources its model — the same
create|adopt idiom the rest of the platform uses: a stable route interface
either way, with the source-specific fields validated at the CRD boundary
rather than silently ignored.
  - foundation: a Bedrock foundation model or inference profile. modelFamily
    is required; crossRegionProfile is available.
  - imported: an open-weight model brought in through Bedrock Custom Model
    Import. modelId is the imported-model ARN; modelFamily and
    crossRegionProfile do not apply and are rejected.

_Validation:_
- Enum: [foundation imported]

_Appears in:_
- [ModelRouteSpec](#modelroutespec)

| Field | Description |
| --- | --- |
| `foundation` | ModelSourceFoundation routes to a Bedrock foundation model / inference profile.<br /> |
| `imported` | ModelSourceImported routes to a Custom Model Import open-weight model by ARN.<br /> |


#### RouteAPI

_Underlying type:_ _string_

RouteAPI is the client-facing wire format a caller speaks to a route.

It is not the same question as which upstream schema serves the route. A
caller reaches every route through the same gateway, but the gateway serves
each wire format under its own endpoint prefix, so the format decides the
URL the caller must use — and which model families it can reach at all.

  - Anthropic: native Anthropic Messages, at `<endpoint>/anthropic`. Keeps
    the model's own shape end to end — thinking blocks, cache points, and
    tool use survive. Anthropic-family foundation routes only.
  - OpenAI: OpenAI chat completions and embeddings, at `<endpoint>/v1`. The
    gateway translates to Bedrock. Reaches every family, which is what makes
    a route repointable to a non-Anthropic model without touching the app.

_Validation:_
- Enum: [Anthropic OpenAI]

_Appears in:_
- [ModelRouteSpec](#modelroutespec)
- [RouteStatus](#routestatus)

| Field | Description |
| --- | --- |
| `Anthropic` | RouteAPIAnthropic is the native Anthropic Messages wire format.<br /> |
| `OpenAI` | RouteAPIOpenAI is the OpenAI chat-completions / embeddings wire format.<br /> |


#### RouteStatus



RouteStatus is the published client contract for one route: what to call it
with, and where.



_Appears in:_
- [ModelGatewayStatus](#modelgatewaystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the route name callers send as the model field. |  |  |
| `api` _[RouteAPI](#routeapi)_ | API is the resolved wire format — spec.routes[].api when set, otherwise<br />derived from the model family. |  | Enum: [Anthropic OpenAI] <br /> |
| `baseURL` _string_ | BaseURL is the base a client of that wire format is configured with. It<br />is not status.endpoint: the gateway serves each format under its own<br />prefix, so the endpoint alone is not a usable base for any client. An<br />Anthropic SDK appends /v1/messages to this, an OpenAI one appends<br />/chat/completions or /embeddings. |  |  |


#### SandboxPool



SandboxPool is a Platform-scoped pool of Managed Agents self-hosted
sandbox workers. The reconciler runs them as a Deployment on the
dedicated, tainted sandbox node pool, locked down by a default-deny
NetworkPolicy.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `SandboxPool` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SandboxPoolSpec](#sandboxpoolspec)_ |  |  |  |
| `status` _[SandboxPoolStatus](#sandboxpoolstatus)_ |  |  |  |


#### SandboxPoolSpec



SandboxPoolSpec declares a pool of Managed Agents self-hosted sandbox
workers for a `self_hosted` environment. The workers run Anthropic's
`ant beta:worker`, claiming sessions from the environment's work queue
and executing agent tool calls inside the cluster.



_Appears in:_
- [SandboxPool](#sandboxpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ | PlatformRef is the owning Platform. The pool's workers run in that<br />Platform's tenant namespace and the pool gates on Platform readiness. |  |  |
| `environmentId` _string_ | EnvironmentID is the Managed Agents self_hosted environment whose<br />work queue these workers drain (an `env_...` id). |  |  |
| `environmentKeySecret` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#secretkeyselector-v1-core)_ | EnvironmentKeySecret holds ANTHROPIC_ENVIRONMENT_KEY — the worker's<br />auth token, mounted into every worker pod. |  |  |
| `apiKeySecret` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#secretkeyselector-v1-core)_ | APIKeySecret holds the organization API key. It is consumed only by<br />the work-queue autoscaler, never mounted into worker pods — Anthropic<br />warns the org key must not be reachable by agent tool calls. |  | Optional: \{\} <br /> |
| `image` _string_ | Image overrides the sandbox worker image. Defaults to the platform's<br />published sandbox-worker image when empty. |  | Optional: \{\} <br /> |
| `scaling` _[SandboxScalingSpec](#sandboxscalingspec)_ | Scaling bounds the worker count. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_ | Resources are the per-worker-pod resource requests and limits. |  | Optional: \{\} <br /> |
| `runtimeClassName` _string_ | RuntimeClassName selects a Kubernetes RuntimeClass for the worker<br />pods — typically "gvisor" or "kata" for kernel-level isolation of<br />the untrusted agent tool code. The named RuntimeClass must already<br />exist in the cluster. Empty uses the cluster's default runtime. |  | Optional: \{\} <br /> |


#### SandboxPoolStatus



SandboxPoolStatus reports the pool's reconciled state.



_Appears in:_
- [SandboxPool](#sandboxpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Ready, Suspended, Failed. |  | Optional: \{\} <br /> |
| `readyWorkers` _integer_ | ReadyWorkers is the worker Deployment's ready replica count. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | ObservedGeneration is the last spec.generation reconciled. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


#### SandboxScalingSpec



SandboxScalingSpec bounds the worker Deployment's replica count.



_Appears in:_
- [SandboxPoolSpec](#sandboxpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `minReplicas` _integer_ | MinReplicas is the worker-count floor. A pointer so 0 (scale to zero,<br />for the autoscaled path) is distinguishable from "field absent". | 1 | Minimum: 0 <br />Optional: \{\} <br /> |
| `maxReplicas` _integer_ | MaxReplicas is the worker-count ceiling for the autoscaler. | 10 | Minimum: 1 <br />Optional: \{\} <br /> |
| `queueDepthTarget` _integer_ | QueueDepthTarget is the work-queue depth per worker the autoscaler<br />aims for before adding workers. | 5 | Minimum: 1 <br />Optional: \{\} <br /> |


#### ScalingSpec



ScalingSpec configures KEDA.



_Appears in:_
- [AgentFleetSpec](#agentfleetspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled — when false, the operator scales the Deployment to 0 and<br />removes the ScaledObject. Toggled false by the kill-switch on budget<br />breach. | true |  |
| `min` _integer_ | Min replicas. Use a pointer so 0 (kill-switch state) is distinguishable<br />from "field absent" — with int32 + omitempty, the zero value gets<br />dropped and re-defaulted, making min=0 unrepresentable. | 1 | Minimum: 0 <br />Optional: \{\} <br /> |
| `max` _integer_ | Max replicas. | 10 | Minimum: 1 <br />Optional: \{\} <br /> |
| `queueDepthTrigger` _integer_ | QueueDepthTrigger: scale up when SQS depth exceeds this value. | 10 | Minimum: 1 <br />Optional: \{\} <br /> |
| `queueUrl` _string_ | QueueUrl is the SQS queue the fleet's work originates from. When<br />set the operator emits a KEDA aws-sqs-queue trigger; otherwise a<br />CPU-utilization placeholder. The tenant role must have<br />sqs:GetQueueAttributes on this queue (granted via the agent-iam<br />baseline policy + an in-policy resource ARN derived from the URL). |  | Pattern: `^https://sqs\.[a-z0-9-]+\.amazonaws\.com/[0-9]\{12\}/[A-Za-z0-9_-]+(\.fifo)?$` <br />Optional: \{\} <br /> |



## governance.nanohype.dev/v1alpha1

Package v1alpha1 contains API Schema definitions for the governance v1alpha1 API group.

### Resource Types
- [BudgetPolicy](#budgetpolicy)
- [EvalSuite](#evalsuite)
- [SLOPolicy](#slopolicy)



#### BudgetPolicy



BudgetPolicy caps monthly spend per Platform and triggers the kill-switch at 120% of the threshold.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `governance.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `BudgetPolicy` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BudgetPolicySpec](#budgetpolicyspec)_ |  |  |  |
| `status` _[BudgetPolicyStatus](#budgetpolicystatus)_ |  |  |  |


#### BudgetPolicySpec



BudgetPolicySpec sets monthly spend caps per Platform.



_Appears in:_
- [BudgetPolicy](#budgetpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ |  |  |  |
| `monthlyUsd` _string_ | MonthlyUsd is the soft threshold expressed as a decimal-string USD amount<br />(e.g. "2500", "1500.50"). KillSwitch fires at 120% of this. Modeled as<br />string for symmetry with Status.CurrentSpendUsd and so future v1 can<br />support fractional cents without a lossy int32 → string conversion. The<br />pattern enforces non-negative decimal with optional 2-digit fraction. |  | MinLength: 1 <br />Pattern: `^[0-9]+(\.[0-9]\{1,2\})?$` <br /> |
| `alertThresholdsPercent` _integer array_ | AlertThresholdsPercent — fire WarnEvent at these % of the threshold. | [50 80 100] | Optional: \{\} <br /> |
| `killSwitchEnabled` _boolean_ | KillSwitchEnabled — when false, breach at 120% is logged but not acted on.<br />Use sparingly; SOC2 platforms must keep this true. | true |  |


#### BudgetPolicyStatus



BudgetPolicyStatus surfaces the latest spend reading. The budget reconciler
updates this on every tick (hourly in prod, 5m in dev) with current spend,
percent-of-budget, the alert thresholds crossed, and reconcile conditions.



_Appears in:_
- [BudgetPolicy](#budgetpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `currentSpendUsd` _string_ | CurrentSpendUsd is the most recent spend snapshot. |  | Optional: \{\} <br /> |
| `percentOfBudget` _integer_ | PercentOfBudget — 0..200+. |  | Optional: \{\} <br /> |
| `lastReconciled` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | LastReconciled timestamp. |  | Optional: \{\} <br /> |
| `killSwitchFiredAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | KillSwitchFiredAt — non-null once the kill-switch has published a<br />breach event. Firing is not the same as taking effect: the platform is<br />suspended by an out-of-band EventBridge→StepFunctions path, and the<br />reconciler confirms the effect (platform observed Suspended) before it<br />treats the switch as done. See KillSwitchRefireCount and the<br />KillSwitchUnrouted condition. |  | Optional: \{\} <br /> |
| `killSwitchRefireCount` _integer_ | KillSwitchRefireCount is how many times the breach event has been<br />re-published because the platform was not observed Suspended within the<br />grace window. Bounded — after the cap the reconciler stops re-publishing<br />but keeps the KillSwitchUnrouted condition set so the alert stays lit. |  | Optional: \{\} <br /> |
| `killSwitchLastRefireAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | KillSwitchLastRefireAt is the timestamp of the most recent re-publish.<br />It anchors the exponential backoff between re-fires. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


#### EvalCase



EvalCase is a single test case. The assertion fields it sets determine its
kind — the runner has no separate discriminator:

  - Golden case: sets ExpectContains (and optionally MaxLatencyMs /
    MaxCostUsd). Passes when the agent's output contains every listed
    substring and stays within the latency/cost ceilings.
  - Adversarial / injection case: sets ExpectNotContains and/or
    ExpectRefusal. Passes when the output leaks none of the forbidden
    substrings and — when ExpectRefusal is set — the agent declined
    (a guardrail intervened, or the output matched a refusal).

A case may combine both families (e.g. a jailbreak attempt that must be
refused AND must not echo a secret). All assertions present must hold.



_Appears in:_
- [EvalSuiteSpec](#evalsuitespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `input` _string_ |  |  |  |
| `expectContains` _string array_ | ExpectContains: the output must contain every one of these substrings<br />(golden / positive assertion). Empty = no positive-content assertion. |  | Optional: \{\} <br /> |
| `expectNotContains` _string array_ | ExpectNotContains: the output must contain none of these substrings<br />(adversarial / data-leak assertion — e.g. a secret, PII, or a phrase<br />that would indicate the agent complied with an injection). Empty = no<br />forbidden-content assertion. |  | Optional: \{\} <br /> |
| `expectRefusal` _boolean_ | ExpectRefusal: when true, the case passes only if the agent declined —<br />either the model gateway reported a guardrail intervention, or the<br />output matched a refusal. Use for adversarial prompts that should be<br />blocked rather than answered. |  | Optional: \{\} <br /> |
| `maxLatencyMs` _integer_ | MaxLatencyMs: if set (>0), the case fails when the observed round-trip<br />latency exceeds this ceiling. |  | Optional: \{\} <br /> |
| `maxCostUsd` _string_ | MaxCostUsd: if set, the case fails when the observed per-call cost<br />exceeds this ceiling. A model with no pricing entry (unpriced) fails<br />this assertion closed rather than passing on a misleading $0. |  | Optional: \{\} <br /> |


#### EvalSuite



EvalSuite is a scheduled evaluation run against an AgentFleet's agents.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `governance.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `EvalSuite` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EvalSuiteSpec](#evalsuitespec)_ |  |  |  |
| `status` _[EvalSuiteStatus](#evalsuitestatus)_ |  |  |  |


#### EvalSuiteSpec



EvalSuiteSpec defines a periodic evaluation run against an AgentFleet.



_Appears in:_
- [EvalSuite](#evalsuite)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ |  |  |  |
| `agentFleetRef` _[LocalRef](#localref)_ | AgentFleetRef targets the fleet whose agents are under test. |  |  |
| `schedule` _string_ | Schedule (cron) — when to run the suite. Empty = manual only. |  | Optional: \{\} <br /> |
| `cases` _[EvalCase](#evalcase) array_ | Cases is the list of test cases (input prompt + expected criteria).<br />In production these are typically loaded from an S3 manifest; this<br />inline list is for small / dev suites. |  | Optional: \{\} <br /> |
| `casesFromManifest` _string_ | CasesFromManifest loads from `eval-reports/<platform>/manifests/<name>.json`<br />in the eval-reports S3 bucket. |  | Optional: \{\} <br /> |
| `passThreshold` _string_ | PassThreshold (0..1) is the required mean score for the run to be<br />marked passing. Argo Rollouts AnalysisTemplate consumes this signal.<br />Modeled as a string so reviewers see decimals in `kubectl get -o yaml`<br />without int<->float coercion surprises; pattern enforces 0.0 .. 1.0. | 0.85 | Pattern: `^(0(\.[0-9]+)?\|1(\.0+)?)$` <br /> |


#### EvalSuiteStatus



EvalSuiteStatus reports the latest run.



_Appears in:_
- [EvalSuite](#evalsuite)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `lastRunAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | LastRunAt timestamp. |  | Optional: \{\} <br /> |
| `lastScore` _string_ | LastScore (mean across cases, 0..1). |  | Optional: \{\} <br /> |
| `lastReportUrl` _string_ | LastReportURL (s3:// URL to the rendered HTML report). |  | Optional: \{\} <br /> |
| `phase` _string_ | Phase: Pending, Running, Passed, Failed. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


#### SLI



SLI declares the good-events/valid-events ratio an objective is measured on.
The reconciler builds the PromQL from these fields rather than accepting a
query — a raw expression would be an injection seam into a request the
operator signs with the platform's own credentials, and the shapes are fully
determined by the observability-slo standard's sli_types anyway.



_Appears in:_
- [SLOPolicySpec](#slopolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type selects the ratio shape. availability divides an errors counter by a<br />requests counter; latency divides a duration histogram's under-threshold<br />bucket by its count. |  | Enum: [availability latency] <br /> |
| `metric` _string_ | Metric is the base series name in Prometheus form — the OTLP service name<br />with dashes normalized to underscores, without the _errors_total /<br />_requests_total / _request_duration_seconds_* suffix the Type implies<br />(e.g. "incident_response_webhook"). |  | MaxLength: 200 <br />Pattern: `^[a-zA-Z_][a-zA-Z0-9_]*$` <br /> |
| `selector` _object (keys:string, values:string)_ | Selector narrows the SLI to a subset of series as an exact-match label<br />set. Rendered into the query as label="value" with values escaped. Keys<br />are Prometheus label names; a raw matcher string is deliberately not<br />accepted. |  | Optional: \{\} <br /> |
| `errorSelector` _object (keys:string, values:string)_ | ErrorSelector narrows the SAME series the denominator counts down to its<br />error subset, for services that emit one dimensioned counter rather than<br />a separate errors counter. With it set, an availability SLI reads<br />	<metric>_requests_total\{<selector>,<errorSelector>\}<br />	  over <metric>_requests_total\{<selector>\}<br />rather than dividing a distinct _errors_total series by _requests_total.<br />That is how a counter with a `status` dimension is normally instrumented,<br />and without this the only way to get an availability objective was to emit<br />a second, redundant counter whose sole purpose was to satisfy the query<br />shape.<br />Same rules as Selector: exact-match label names, values escaped, `le` and<br />`__name__` reserved. Availability only — a latency SLI's numerator is a<br />bucket boundary, not a label selection.<br />One limitation, stated because it is the failure this field can produce.<br />The query defaults an empty numerator to zero only when every key named<br />here is present on the series, so a misspelled KEY reads NoData rather<br />than a permanent healthy zero. A misspelled VALUE cannot be distinguished<br />from a service that is genuinely not erroring, and reads zero. Confirm the<br />selector against the metric store once when authoring the objective. |  | Optional: \{\} <br /> |
| `thresholdSeconds` _string_ | ThresholdSeconds is the histogram bucket boundary a latency SLI counts as<br />good, as a decimal-string of seconds ("0.5"). Required for<br />type=latency, ignored for type=availability. It must name a bucket the<br />histogram actually publishes — an le value with no matching bucket yields<br />an empty result, which the reconciler reports as NoData rather than as a<br />healthy zero. |  | Pattern: `^[0-9]+(\.[0-9]\{1,6\})?$` <br />Optional: \{\} <br /> |


#### SLOPolicy



SLOPolicy declares a Platform's service-level objective and turns a page-tier
error-budget burn into a platform action: an event on the kill-switch bus and
a hold on the tenant's rollout.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `governance.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `SLOPolicy` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SLOPolicySpec](#slopolicyspec)_ |  |  |  |
| `status` _[SLOPolicyStatus](#slopolicystatus)_ |  |  |  |


#### SLOPolicySpec



SLOPolicySpec declares one service-level objective for a Platform and what
the control loop does when its error budget burns too fast.

The threshold rules are validated here rather than left to the reconciler
because both failure modes are permanent and quiet: a latency SLI with no
threshold builds an invalid query on every tick forever, and a threshold on an
availability SLI is silently ignored, so the author believes they narrowed an
objective that is in fact measuring everything.



_Appears in:_
- [SLOPolicy](#slopolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ |  |  |  |
| `sli` _[SLI](#sli)_ | SLI is the ratio this objective is measured on. |  |  |
| `objective` _string_ | Objective is the target good-event ratio as a decimal string ("0.999").<br />The error budget is 1 - Objective, and the burn rate is the observed error<br />ratio over a window divided by that budget. Modeled as a string for the<br />same reason BudgetPolicy.monthlyUsd is: a float64 round-trip through JSON<br />would perturb the denominator every burn-rate alert divides by. Bounded<br />below 1 because an objective of 1 leaves a zero budget and an infinite<br />burn rate. |  | Pattern: `^0\.[0-9]\{1,6\}$` <br /> |
| `onPageTierBreach` _string_ | OnPageTierBreach is the automated action taken when a page-tier burn-rate<br />window pair trips.<br />	HoldRollout — patch a deny syncWindow onto the tenant's ArgoCD<br />	              AppProject so a bad rollout stops advancing. The window<br />	              leaves manualSync open, so an operator can still push a<br />	              fix by hand. Reversed automatically once the burn clears.<br />	None        — evaluate, publish, and page, but take no cluster action.<br />The default is None because HoldRollout is only an action for a tenant<br />that has its own AppProject. The hold is written to the AppProject named<br />for the Platform, and a tenant synced under the shared `platform` project<br />— which is every ApplicationSet on the default path — has nothing for it<br />to write to. Such a policy reports RolloutHeld=Unknown/AppProjectAbsent,<br />which is honest but arrives after the fact; defaulting to the action that<br />cannot happen makes the API claim more than it delivers.<br />Set HoldRollout deliberately, on a tenant whose Applications resolve to a<br />per-Platform AppProject. | None | Enum: [HoldRollout None] <br />Optional: \{\} <br /> |


#### SLOPolicyStatus



SLOPolicyStatus surfaces the most recent burn-rate evaluation. The SLO
reconciler rewrites it on every tick. It is the operator's single evaluation
of this objective: kube-state-metrics projects these fields, so the paging
alert reads the number computed here instead of re-deriving the same PromQL
against the same data.



_Appears in:_
- [SLOPolicy](#slopolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `errorRatios` _object (keys:string, values:string)_ | ErrorRatios is the observed error ratio for each queried window, keyed by<br />the window ("5m", "1h", "3d", "30d", …) with a decimal-string value.<br />Present so an operator can see which window tripped, and by how much,<br />without re-running the queries by hand. |  | Optional: \{\} <br /> |
| `pageTierBreachRatio` _string_ | PageTierBreachRatio is how close the page tier is to breaching, as a<br />decimal string, normalized so one threshold covers every window: across<br />each page-tier window pair it takes min(long burn rate, short burn rate)<br />divided by that pair's factor, and reports the largest. 1.0 means a pair<br />is exactly at its factor; above 1.0 is a breach. Normalized rather than<br />raw because the page tier has two pairs with different factors (14.4 and<br />6), so no single raw burn rate can be compared against one number. |  | Optional: \{\} <br /> |
| `ticketTierBreachRatio` _string_ | TicketTierBreachRatio is the same normalized measure over the ticket-tier<br />window pairs (factors 3 and 1). |  | Optional: \{\} <br /> |
| `errorBudgetRemaining` _string_ | ErrorBudgetRemaining is the fraction of the SLO window's error budget<br />still unspent, as a decimal string clamped to [0,1]: 1 - (error ratio over<br />the 30d SLO window / error budget). Measured over the full window the<br />standard defines rather than extrapolated from a burn window, so the<br />gauge means what it says. |  | Optional: \{\} <br /> |
| `breachedWindow` _string_ | BreachedWindow names the long window whose pair tripped at the highest<br />severity ("1h", "6h", "1d", "3d"). Empty when nothing is breaching. |  | Optional: \{\} <br /> |
| `severity` _string_ | Severity is the tier of the current breach: critical for a page-tier<br />window pair, warning for a ticket-tier pair, empty when healthy. |  | Enum: [critical warning ] <br />Optional: \{\} <br /> |
| `lastReconciled` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | LastReconciled is when the reconciler last ticked, whether or not it got a<br />usable reading. |  | Optional: \{\} <br /> |
| `lastEvaluated` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | LastEvaluated is when the objective was last actually measured — set only<br />on a tick that obtained a reading. It is deliberately distinct from<br />LastReconciled: a reconciler that ticks every five minutes and fails every<br />AMP query keeps LastReconciled fresh forever, so a staleness alert built on<br />that field can never fire. This is the field that answers "is this<br />objective being measured", which is the question worth alerting on. |  | Optional: \{\} <br /> |
| `breachFiredAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | BreachFiredAt is when the current unbroken breach episode was first<br />published to the kill-switch bus. Cleared when the burn clears, so a<br />later breach publishes again rather than being swallowed as a duplicate. |  | Optional: \{\} <br /> |
| `holdEngagedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | HoldEngagedAt is when this reconciler decided to hold the tenant's<br />rollout. It is the decision, not the effect: the Platform reconciler is<br />the only writer of the AppProject, and it renders the deny syncWindow from<br />this field. Non-null means a hold is called for. |  | Optional: \{\} <br /> |
| `holdObservedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | HoldObservedAt is when the deny syncWindow was last actually seen on the<br />tenant's AppProject. Engaging a hold is not the same as holding: if this<br />stays null past the grace window while HoldEngagedAt is set, the hold is<br />not landing and the HoldUnobserved condition says so. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |



## platform.nanohype.dev/v1alpha1

Package v1alpha1 contains API Schema definitions for the platform v1alpha1 API group.

### Resource Types
- [Platform](#platform)
- [Tenant](#tenant)



#### AttributeSchema



AttributeSchema names a DynamoDB key attribute and its scalar type
(S string, N number, B binary).



_Appears in:_
- [GlobalSecondaryIndex](#globalsecondaryindex)
- [KeyValueConfig](#keyvalueconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the attribute. |  | Pattern: `^[a-zA-Z0-9_.-]\{1,255\}$` <br /> |
| `type` _string_ | Type is the DynamoDB scalar attribute type. |  | Enum: [S N B] <br /> |


#### AttributionSpec



AttributionSpec configures per-session human attribution for a Platform. See
github.com/nanohype/fab docs/attribution.md for the consumer side.



_Appears in:_
- [PlatformSpec](#platformspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `operators` _string array_ | Operators is the set of human identities (e.g. email addresses) a<br />session in this Platform may act as. Each value becomes both an allowed<br />STS SourceIdentity on the session role's trust policy and a resourceNames<br />entry on the impersonate ClusterRole, so the SAME string binds the AWS<br />and Kubernetes audit records. Use a canonical form (a lowercased email);<br />it must byte-match the operator's own RBAC subject name. |  | MinItems: 1 <br /> |
| `sessionRoleMaxDurationSeconds` _integer_ | SessionRoleMaxDurationSeconds caps the assumed session lifetime. Because<br />the caller is the tenant role, AWS STS role chaining hard-caps a<br />chained session at 3600s regardless of this value; larger values only<br />matter if the caller ever changes. Defaults to 3600. | 3600 | Maximum: 43200 <br />Minimum: 900 <br />Optional: \{\} <br /> |


#### BudgetRef



BudgetRef points at a BudgetPolicy by name.



_Appears in:_
- [PlatformSpec](#platformspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |


#### CacheConfig



CacheConfig tunes the ElastiCache cluster. Changing engine or node type on a
live cluster is a disruptive replacement, so treat both as set-at-create.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `engine` _string_ | Engine of the cache. Valkey is the default going-forward OSS engine. | valkey | Enum: [valkey redis] <br />Optional: \{\} <br /> |
| `nodeType` _string_ | NodeType sizes each node. Changing it on a live cluster is a disruptive<br />resize. | cache.t4g.micro | Optional: \{\} <br /> |
| `replicas` _integer_ | Replicas is the number of read replicas per shard; 0 (default) is a<br />single-node cache for a young tenant. | 0 | Maximum: 5 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### Capability

_Underlying type:_ _string_

Capability is a managed AWS capability the datastore vocabulary does not
cover. Declaring one drives an operator-generated `capability-access` inline
policy on the tenant role, scoped by the same <env>-<platform> naming
convention the datastore policy uses — so a capability is a statement of
need, not a hand-written managed policy the tenant references by ARN.

	ses                  -> ses:SendEmail scoped by a ses:FromAddress condition
	                        to the tenant's sending domain. The verified sending
	                        identity itself is account-level mail infra
	                        (landing-zone), not provisioned here.
	eventBridgeScheduler -> scheduler:*Schedule on the tenant's own schedules
	                        plus an operator-minted <cluster>-<platform>-scheduler-invoke
	                        role (trusted by scheduler.amazonaws.com, allowed to
	                        SendMessage to the tenant's own queue datastores) that
	                        the tenant passes when creating a schedule.

_Validation:_
- Enum: [ses eventBridgeScheduler]

_Appears in:_
- [IdentitySpec](#identityspec)

| Field | Description |
| --- | --- |
| `ses` |  |
| `eventBridgeScheduler` |  |


#### ComplianceSpec



ComplianceSpec is the compliance posture a Platform declares.

These are declarations, not switches. This operator provisions nothing
differently because a flag here is set; `cloudgov platform audit` is what
reads them, checking the rest of the declaration is consistent with the
posture and reporting a finding where it is not.

The controls themselves are substrate and run for every Platform regardless:
Bedrock invocation logging, the EventBridge archive, CloudTrail, CMK
encryption at rest, and per-tenant isolation. Running a regulated workload
takes more than a flag here — HIPAA in particular requires a BAA with AWS.



_Appears in:_
- [PlatformSpec](#platformspec)
- [TenantSpec](#tenantspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hipaa` _boolean_ | HIPAA marks the Platform as handling PHI. One audited invariant: a<br />Platform whose Tenant sets hipaa must set it too. |  | Optional: \{\} <br /> |
| `soc2` _boolean_ | SOC2 marks the Platform as in SOC 2 audit scope. Two audited invariants:<br />the referenced BudgetPolicy must have killSwitchEnabled, and a Platform<br />whose Tenant sets soc2 must set it too. |  | Optional: \{\} <br /> |


#### ContactSpec



ContactSpec carries owner / on-call / billing reach paths.



_Appears in:_
- [TenantSpec](#tenantspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `slackChannel` _string_ | SlackChannel for tenant-wide notifications (e.g. "#acme-ops"). |  | Optional: \{\} <br /> |
| `oncallRotation` _string_ | OncallRotation — Pagerduty schedule key or similar identifier. |  | Optional: \{\} <br /> |
| `billingEmail` _string_ | BillingEmail — invoice + budget-breach notification recipient. |  | Optional: \{\} <br /> |


#### DatastoreKind

_Underlying type:_ _string_

DatastoreKind is the abstract kind of a tenant datastore. The Platform CR
names what the tenant needs; the operator and the tenant-substrate tofu
module map each kind to an AWS implementation and scope access to it. Keeping
the vocabulary abstract preserves the pluggable seam the org commits to
elsewhere and keeps the spec a statement of need rather than a config file
for a specific service.

	relational  -> Aurora PostgreSQL Serverless v2
	keyValue    -> DynamoDB
	objectStore -> S3
	queue       -> SQS (with a dead-letter queue when redrive is set)
	cache       -> ElastiCache (Valkey / Redis)
	stream      -> MSK Serverless (IAM auth)

_Validation:_
- Enum: [relational keyValue objectStore queue cache stream]

_Appears in:_
- [DatastoreSpec](#datastorespec)
- [DatastoreStatus](#datastorestatus)

| Field | Description |
| --- | --- |
| `relational` |  |
| `keyValue` |  |
| `objectStore` |  |
| `queue` |  |
| `cache` |  |
| `stream` |  |


#### DatastoreSpec



DatastoreSpec declares one stateful store the tenant needs. The kind selects
an AWS implementation and, at most, the one typed config block matching that
kind (stream needs none; a kind whose block is omitted takes the young/light
defaults). The heavy resource is provisioned by the tenant-substrate tofu
module; the operator generates the scoped IAM policy that reaches it. Nothing
here grants the operator delete on the store — deletion is governed by
deletionPolicy and the per-kind deletion_protection backstop, not by the
reconciling principal's IAM (T1/T2).

Drift, stated once here rather than per field, because it is the same answer
for every field: the operator does not observe it. It holds no AWS client for
RDS, DynamoDB, ElastiCache, SQS or MSK, so it cannot read a live datastore's
configuration, let alone converge one. Drift between this declaration and the
provisioned resource is detected where the resource is owned — landing-zone's
scheduled `tofu plan` (`drift.yml`), which opens a GitHub issue. It does not
reach the CR, and no datastore status field reports it.



_Appears in:_
- [PlatformSpec](#platformspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name identifies the datastore within its Platform and composes into the<br />AWS resource names (bucket, table, queue, cluster) alongside the env,<br />account, and platform tokens. A short RFC-1123 label; the tenant-substrate<br />module re-proves the exact composed length at its variable boundary, where<br />the env and account values are known. |  | Pattern: `^[a-z0-9]([a-z0-9-]\{0,16\}[a-z0-9])?$` <br /> |
| `kind` _[DatastoreKind](#datastorekind)_ | Kind selects the AWS implementation. Immutable — changing a live<br />datastore's kind would strand the provisioned resource. |  | Enum: [relational keyValue objectStore queue cache stream] <br /> |
| `deletionPolicy` _string_ | DeletionPolicy declares whether this datastore's AWS resource should<br />survive a teardown of the substrate that provisioned it.<br />It does NOT govern `kubectl delete platform`. The operator holds no delete<br />permission on any datastore, so deleting the CR orphans every store<br />regardless of this field — the isolation boundary is enforced by<br />permission, not by policy. This field reaches only the tenant-substrate<br />module's destroy path.<br />What it means is per-kind, because the same word cannot name the same<br />mechanism across services that offer different ones:<br />  keyValue: Retain arms DynamoDB deletion protection, which refuses the<br />    delete outright and leaves no cost behind. The clean case.<br />  relational: Retain takes a final Aurora snapshot; Delete skips it. The<br />    snapshot outlives the cluster and begins billing per GB-month once it<br />    falls outside backupRetentionDays, so Retain here is durability with a<br />    cost tail, not free protection. Aurora's own deletionProtection is a<br />    separate and independent gate: Delete alone will not drop a protected<br />    cluster, both must open.<br />  objectStore, queue, cache, stream: no effect. S3 has no deletion<br />    protection (force_destroy governs emptying a bucket, not deleting an<br />    empty one), and SQS, ElastiCache and MSK offer no retain lever at all.<br />    Cache is derived data by design and is deliberately unprotected.<br />The operator's substrate-wide teardown lever overrides this in every case. | Retain | Enum: [Retain Delete] <br />Optional: \{\} <br /> |
| `relational` _[RelationalConfig](#relationalconfig)_ | Relational config; honored only when kind=relational. |  | Optional: \{\} <br /> |
| `keyValue` _[KeyValueConfig](#keyvalueconfig)_ | KeyValue config; honored only when kind=keyValue. |  | Optional: \{\} <br /> |
| `objectStore` _[ObjectStoreConfig](#objectstoreconfig)_ | ObjectStore config; honored only when kind=objectStore. |  | Optional: \{\} <br /> |
| `queue` _[QueueConfig](#queueconfig)_ | Queue config; honored only when kind=queue. |  | Optional: \{\} <br /> |
| `cache` _[CacheConfig](#cacheconfig)_ | Cache config; honored only when kind=cache. |  | Optional: \{\} <br /> |


#### DatastoreStatus



DatastoreStatus reports the stable identity a tenant uses to reach one
declared datastore. It lives under PlatformStatus.Datastores.

It is an identity report, not a liveness report. Every value here is composed
from the <env>-<platform>-<datastore> convention that the tenant-substrate
module names by and the datastore-access policy scopes to, so it needs no AWS
call — and makes no claim that the resource exists yet.



_Appears in:_
- [PlatformStatus](#platformstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name matches spec.datastores[].name. |  |  |
| `kind` _[DatastoreKind](#datastorekind)_ | Kind echoes the declared kind. |  | Enum: [relational keyValue objectStore queue cache stream] <br />Optional: \{\} <br /> |
| `phase` _string_ | Phase is the owning Platform's phase, copied. It reports identity and<br />access readiness, not the datastore's own state — the operator observes<br />no live datastore, so it has nothing else to derive a phase from, and<br />this can never hold a value the Platform cannot.<br />Phase: Pending, Provisioning, Ready, Suspended, Failed. |  | Optional: \{\} <br /> |
| `endpoint` _string_ | Endpoint is the connection address, and only for the kinds whose name is<br />fully deterministic: the S3 bucket, the DynamoDB table, the SQS queue URL.<br />Aurora, ElastiCache and MSK endpoints carry an AWS-generated id, so this<br />is empty for those three and the address comes from the module's outputs. |  | Optional: \{\} <br /> |
| `arn` _string_ | ARN of the datastore, composed from the naming convention. |  | Optional: \{\} <br /> |
| `secretName` _string_ | SecretName is the resolved name of the credentials secret the datastore<br />publishes — the RDS-managed master secret for relational — so the tenant<br />chart reads one predictable place instead of hand-wiring it per app (T7). |  | Optional: \{\} <br /> |


#### GlobalSecondaryIndex



GlobalSecondaryIndex declares a DynamoDB GSI. The key schema is immutable
(AWS recreates the index to change it).



_Appears in:_
- [KeyValueConfig](#keyvalueconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the index. |  | Pattern: `^[a-zA-Z0-9_.-]\{3,255\}$` <br /> |
| `partitionKey` _[AttributeSchema](#attributeschema)_ | PartitionKey (hash key) of the index. |  |  |
| `sortKey` _[AttributeSchema](#attributeschema)_ | SortKey (range key) of the index. |  | Optional: \{\} <br /> |
| `projection` _string_ | Projection controls which attributes are copied into the index. | ALL | Enum: [ALL KEYS_ONLY INCLUDE] <br />Optional: \{\} <br /> |


#### IdentitySpec



IdentitySpec wires the per-Platform tenant role. The controller reconciles a
`bedrock-model-scoping` inline policy onto the tenant role (and the
attribution session role, when spec.attribution is set) that denies the
Bedrock model-invoke actions (InvokeModel, InvokeModelWithResponseStream,
Converse, ConverseStream) on every resource outside the set that
AllowedModels / AllowedModelFamilies expand to. The baseline policy's broad
invoke grant is thereby narrowed to exactly the declared models; when
neither field is set the policy denies all model invocation
(deny-by-default).



_Appears in:_
- [PlatformSpec](#platformspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `allowedModels` _string array_ | AllowedModels is the list of Bedrock model IDs or cross-region<br />inference-profile IDs (e.g. "anthropic.claude-sonnet-4-6",<br />"us.anthropic.claude-sonnet-4-6-v1:0") the role may invoke. The<br />controller expands each entry into its foundation-model ARN pattern plus<br />the matching inference-profile ARN pattern (a `us.` profile fans out to<br />foundation models across regions, so both are granted together) and<br />reconciles them into the role's bedrock-model-scoping policy. Scopes<br />tighter than a family; mutually exclusive with AllowedModelFamilies. |  | Optional: \{\} <br /> |
| `allowedModelFamilies` _string array_ | AllowedModelFamilies (e.g. ["anthropic", "amazon-nova"]) is expanded by<br />the controller at reconcile time into the family's foundation-model ARN<br />pattern (arn:<partition>:bedrock:*::foundation-model/<prefix>*) and, for<br />families with cross-region inference profiles (anthropic, amazon-nova,<br />meta, mistral), the `us.` inference-profile ARN pattern<br />(arn:<partition>:bedrock:<region>:<account>:inference-profile/us.<prefix>*),<br />then reconciled into the role's bedrock-model-scoping policy. Leaving<br />both this and AllowedModels empty denies all Bedrock model invocation<br />for the Platform's roles. |  | items:Enum: [anthropic amazon-nova amazon-titan meta mistral cohere stability] <br />Optional: \{\} <br /> |
| `extraPolicyArns` _string array_ | ExtraPolicyArns are managed IAM policies attached on top of the baseline.<br />Every sibling on this struct is bounded — DirectSecretReads by count, length<br />and pattern, Capabilities by count, AllowedModelFamilies by enum — and this<br />one governs which managed IAM policies get attached to the tenant role, so it<br />is the field where an unbounded value costs the most.<br />MaxItems=9 keeps the role inside AWS's default quota. "Managed policies per<br />role" is 10 by default (raisable to 25), and the operator attaches the tenant<br />baseline itself (platform_iam.go reconcileManagedPolicies), so nine declared<br />entries is the most that can be satisfied without a quota increase. Past that<br />the attach fails at reconcile — a LimitExceeded the author never sees, on a<br />Platform that stays Provisioning. Bounding it here puts the failure at<br />admission, where the person who typed it is looking.<br />The pattern is an IAM POLICY arn specifically. Without it this field accepts<br />any string, and the operator hands whatever it holds to AttachRolePolicy —<br />so a typo becomes a reconcile error and an ARN of the wrong service becomes<br />one too, both discovered long after apply. Both partitions and both policy<br />owners are allowed: `arn:aws:iam::aws:policy/ReadOnlyAccess` (AWS-managed) and<br />`arn:aws:iam::123456789012:policy/team/MyPolicy` (customer-managed, path<br />optional). |  | MaxItems: 9 <br />items:MaxLength: 2048 <br />items:Pattern: `^arn:aws[a-z-]*:iam::(aws\|[0-9]\{12\}):policy/[A-Za-z0-9+=,.@_/-]+$` <br />Optional: \{\} <br /> |
| `capabilities` _[Capability](#capability) array_ | Capabilities are managed AWS capabilities outside the datastore vocabulary<br />(SES send, EventBridge Scheduler). Each drives an operator-generated<br />`capability-access` inline policy — and, for eventBridgeScheduler, a minted<br />scheduler-invoke role — so a tenant declares what it needs rather than<br />referencing a hand-written managed policy through extraPolicyArns. |  | Enum: [ses eventBridgeScheduler] <br />MaxItems: 8 <br />Optional: \{\} <br /> |
| `directSecretReads` _string array_ | DirectSecretReads names the application secrets this Platform's pods read<br />directly through the pod role via the AWS SDK, each a name under the<br />tenant's own <platform>/<env>/ prefix in Secrets Manager (e.g.<br />"grafana/oncall-webhook-hmac"). The controller grants<br />secretsmanager:GetSecretValue/DescribeSecret on exactly those secrets in<br />the tenant-secrets inline policy — no prefix wildcard. Secret material<br />projected into the pod by the chart's ExternalSecret is resolved by the<br />External Secrets controller's own identity and needs no entry here.<br />Leaving this empty means the tenant role holds no grant on any *application*<br />secret. It is not the whole Secrets Manager story: a relational datastore<br />separately grants read on the one RDS-managed master secret its own Aurora<br />cluster owns, resolved from the ARN the tenant-substrate component publishes<br />— never a prefix, since every Aurora master secret in the account shares one. |  | MaxItems: 16 <br />items:MaxLength: 256 <br />items:Pattern: `^[A-Za-z0-9][A-Za-z0-9/_+=.@-]*$` <br />Optional: \{\} <br /> |


#### KeyValueConfig



KeyValueConfig tunes the DynamoDB table. The key schema is immutable.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `partitionKey` _[AttributeSchema](#attributeschema)_ | PartitionKey (hash key). Immutable after create. |  |  |
| `sortKey` _[AttributeSchema](#attributeschema)_ | SortKey (range key). Immutable after create. |  | Optional: \{\} <br /> |
| `billingMode` _string_ | BillingMode. PAY_PER_REQUEST (default) suits a young tenant with unknown<br />traffic; PROVISIONED is for steady, predictable load. | PAY_PER_REQUEST | Enum: [PAY_PER_REQUEST PROVISIONED] <br />Optional: \{\} <br /> |
| `ttlAttribute` _string_ | TTLAttribute names the item attribute holding an epoch expiry; empty<br />disables TTL. |  | Optional: \{\} <br /> |
| `pointInTimeRecovery` _boolean_ | PointInTimeRecovery enables continuous backups. Defaults on. | true | Optional: \{\} <br /> |
| `globalSecondaryIndexes` _[GlobalSecondaryIndex](#globalsecondaryindex) array_ | GlobalSecondaryIndexes declared on the table. |  | Optional: \{\} <br /> |


#### ObjectStoreConfig



ObjectStoreConfig tunes the S3 bucket. Encryption and public-access blocking
are always on and not configurable.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `versioning` _boolean_ | Versioning keeps prior object versions. Defaults on; set false only for a<br />bucket of regenerable data where prior versions add cost with no recovery<br />value. | true | Optional: \{\} <br /> |
| `lifecycleExpireDays` _integer_ | LifecycleExpireDays expires objects after N days; 0 (default) keeps them<br />indefinitely. | 0 | Minimum: 0 <br />Optional: \{\} <br /> |


#### Platform



Platform is the top-level tenancy CR. Namespaced so that BudgetPolicy,
ModelGateway, AgentFleet, and EvalSuite references resolve in the same
namespace by name. The operator provisions the tenant workload namespace
(tenants-<platform-name>) separately at reconcile time; the Platform CR
itself lives in whichever namespace the cluster admin places it (typically
a management namespace such as eks-agent-platform).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `platform.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `Platform` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PlatformSpec](#platformspec)_ |  |  |  |
| `status` _[PlatformStatus](#platformstatus)_ |  |  |  |


#### PlatformSpec



PlatformSpec defines the desired state of a Platform — a tenancy boundary
hosting one or more AgentFleets, with its own budget, identity, and
guardrails.



_Appears in:_
- [Platform](#platform)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `displayName` _string_ | DisplayName is a human-readable name for dashboards and CLI output. |  | Optional: \{\} <br /> |
| `persona` _string_ | Persona drives default values for AgentFleet, ModelGateway, and<br />dashboards. One of: sales-ops, support, finance, ops, founder, eng,<br />marketing, legal, generic. | generic | Enum: [sales-ops support finance ops founder eng marketing legal generic] <br /> |
| `tenant` _string_ | Tenant is the owning Tenant CR (one Tenant can own multiple Platforms). |  |  |
| `budget` _[BudgetRef](#budgetref)_ | Budget references a BudgetPolicy CR in the same namespace. |  |  |
| `identity` _[IdentitySpec](#identityspec)_ | Identity controls how the tenant role is named + which Bedrock models are<br />reachable. |  |  |
| `compliance` _[ComplianceSpec](#compliancespec)_ | Compliance is the posture this Platform declares, audited by<br />`cloudgov platform audit` rather than enforced by this operator. |  | Optional: \{\} <br /> |
| `isolation` _string_ | Isolation is the workload-isolation tier:<br />  - namespace (default): namespace RBAC + default-deny NetworkPolicy +<br />    ResourceQuota + PSS-restricted, tenant workloads on the host API server.<br />  - vcluster: the same host-side containment PLUS a per-Platform virtual<br />    cluster, so tenant code that talks to the Kubernetes API talks to its own<br />    API server, not the host's (API-server-level isolation — NOT kernel/node<br />    isolation; see docs/adr/0009-vcluster-isolation-tier.md and SECURITY.md).<br />Immutable: switching tiers on a live Platform is a migration (it would strand<br />the virtual cluster and its synced host objects), so the tier is fixed at<br />create time. Re-declare the Platform to change it. Enforced at admission by<br />the CEL transition rule below — an invalid tier flip fails the apply rather<br />than silently half-reconciling. | namespace | Enum: [namespace vcluster] <br />Optional: \{\} <br /> |
| `attribution` _[AttributionSpec](#attributionspec)_ | Attribution opts the Platform into per-session human attribution. When<br />set, the operator provisions a session role — assumable by the tenant<br />role with the operator carried as STS SourceIdentity, scoped to the<br />tenant baseline (Bedrock invoke) and NOT broad sts:AssumeRole — plus a<br />ClusterRole letting the tenant ServiceAccount impersonate the named<br />operators at the apiserver. fab's role-session entrypoint consumes both,<br />so an agent's AWS + Kubernetes actions attribute to a named human.<br />nil = unattributed (the default). |  | Optional: \{\} <br /> |
| `datastores` _[DatastoreSpec](#datastorespec) array_ | Datastores declares the tenant's stateful substrate — the databases,<br />buckets, queues, caches, and streams it needs. Each entry is a declaration,<br />not a hand-written component: the tenant-substrate tofu module provisions<br />the heavy resource from this same list and the operator generates the<br />scoped IAM policy that reaches it, so adding a tenant never means authoring<br />a landing-zone component. Empty for a Platform with no stateful needs. |  | MaxItems: 24 <br />Optional: \{\} <br /> |


#### PlatformStatus



PlatformStatus captures the controller's view of the world.



_Appears in:_
- [Platform](#platform)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Provisioning, Ready, Suspended, Failed. |  | Optional: \{\} <br /> |
| `iamRoleArn` _string_ | IamRoleArn is the per-Platform tenant role created by the controller.<br />Tenant pods receive its credentials through the Pod Identity association<br />reported in status.podIdentity. |  | Optional: \{\} <br /> |
| `sessionRoleArn` _string_ | SessionRoleArn is the per-Platform attribution session role, created when<br />spec.attribution is set. Empty when attribution is off. |  | Optional: \{\} <br /> |
| `namespace` _string_ | Namespace is the tenant namespace the controller provisioned. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | ObservedGeneration is the last spec.generation the controller reconciled. |  | Optional: \{\} <br /> |
| `suspendedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | SuspendedAt is the timestamp at which the kill-switch fired. When<br />non-nil the operator stops reattaching the baseline IAM policy and<br />the AgentFleetReconciler scales fleets to zero. Resets to nil only<br />when ops clears the iam:TagRole 'platform.nanohype.dev/suspended'<br />marker on the tenant role. |  | Optional: \{\} <br /> |
| `suspendedReason` _string_ | SuspendedReason carries the kill-switch's reason (e.g.<br />'budget-exceeded'). Same lifecycle as SuspendedAt. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ | Conditions follows the standard kubernetes pattern. |  | Optional: \{\} <br /> |
| `datastores` _[DatastoreStatus](#datastorestatus) array_ | Datastores reports per-datastore observed state, separate from the<br />top-level Phase: a Platform is Ready once its namespace, quota, and<br />identity are live, while each datastore reports its own readiness here so a<br />still-creating Aurora cluster does not gate the tenant's Ready (T6). |  | Optional: \{\} <br /> |
| `podIdentity` _[PodIdentityStatus](#podidentitystatus)_ | PodIdentity reports the EKS Pod Identity association that binds this<br />tenant's ServiceAccount to status.iamRoleArn. Empty until the association<br />is reconciled (and on the suspended path, which skips AWS writes — the<br />last observed binding is preserved rather than cleared). |  | Optional: \{\} <br /> |


#### PodIdentityStatus



PodIdentityStatus is the observed (namespace, serviceAccount) → role binding.

It exists because the binding is not derivable from anything else on the CR.
The trust policy carries no subject — under Pod Identity the constraint lives
in the association, not the role — and the ServiceAccount the association
targets is not always tenant-runtime: under spec.isolation: vcluster the
syncer rewrites it to a translated host name whose algorithm (and hash
truncation) is vcluster's, not ours. Publishing what the operator actually
bound means an auditor reads one field instead of reimplementing that.



_Appears in:_
- [PlatformStatus](#platformstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterName` _string_ | ClusterName is the EKS cluster the association lives on. Associations are<br />cluster-scoped resources, so this is required to look one up. |  | Optional: \{\} <br /> |
| `namespace` _string_ | Namespace is the host namespace the association targets — the tenant<br />namespace on both isolation tiers. |  | Optional: \{\} <br /> |
| `serviceAccount` _string_ | ServiceAccount is the host ServiceAccount name the association targets:<br />tenant-runtime under namespace isolation, the vcluster-translated host<br />name under vcluster isolation. |  | Optional: \{\} <br /> |
| `roleArn` _string_ | RoleArn is the role the association vends. Always equal to<br />status.iamRoleArn at the moment it was written; reported separately so a<br />reader can tell a stale binding from a current one. |  | Optional: \{\} <br /> |


#### QueueConfig



QueueConfig tunes the SQS queue. FIFO-ness is immutable (a FIFO and a standard
queue are different resources).



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `fifo` _boolean_ | FIFO makes an exactly-once, ordered queue. Immutable after create. | false | Optional: \{\} <br /> |
| `visibilityTimeoutSeconds` _integer_ | VisibilityTimeoutSeconds before a received-but-unacked message is<br />redelivered. | 30 | Maximum: 43200 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `messageRetentionSeconds` _integer_ | MessageRetentionSeconds a message is kept before it expires (default 4<br />days). | 345600 | Maximum: 1.2096e+06 <br />Minimum: 60 <br />Optional: \{\} <br /> |
| `maxReceiveCount` _integer_ | MaxReceiveCount, when > 0, provisions a dead-letter queue and redrives a<br />message to it after this many failed receives; 0 (default) means no DLQ. | 0 | Maximum: 1000 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### RelationalConfig



RelationalConfig tunes the Aurora PostgreSQL Serverless v2 cluster. Omitting
the block provisions the young/light default: 0.5–8 ACU, 7-day backups,
deletion protection on.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `engineVersion` _string_ | EngineVersion of Aurora PostgreSQL. Auto-pause (minACU "0") needs 16.3 or<br />later, which the rule on this type enforces against the major version —<br />the minor is not machine-checkable here, so 16.0–16.2 is admitted and<br />fails at apply rather than at admission. | 16.6 | MaxLength: 10 <br />Pattern: `^[0-9]+\.[0-9]+$` <br />Optional: \{\} <br /> |
| `minACU` _string_ | MinACU is the Serverless v2 floor in Aurora Capacity Units, in 0.5-ACU<br />steps (e.g. "0.5", "1", "8"). Serialized as a string, per the Kubernetes<br />convention for fractional values.<br />"0" is the auto-pause floor: the cluster scales to zero compute after five<br />idle minutes and a later connection resumes it, at the cost of roughly<br />fifteen seconds on that first connect. It pauses only while nothing is<br />connected, so a workload holding a pool open — or probing readiness with a<br />query — never reaches it and bills the same as "0.5". Storage, IO, backup<br />and KMS bill through a pause regardless; only compute stops. | 0.5 | MaxLength: 5 <br />Pattern: `^(0\|0\.5\|([1-9]\|[1-9][0-9]\|1[0-9]\{2\}\|2[0-4][0-9]\|25[0-5])(\.5)?\|256)$` <br />Optional: \{\} <br /> |
| `maxACU` _string_ | MaxACU is the Serverless v2 ceiling, in 0.5-ACU steps. Unlike the floor it<br />cannot be "0" — a ceiling of zero leaves no capacity to scale into. | 8 | MaxLength: 5 <br />Pattern: `^(0\.5\|([1-9]\|[1-9][0-9]\|1[0-9]\{2\}\|2[0-4][0-9]\|25[0-5])(\.5)?\|256)$` <br />Optional: \{\} <br /> |
| `backupRetentionDays` _integer_ | BackupRetentionDays for automated backups. | 7 | Maximum: 35 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `deletionProtection` _boolean_ | DeletionProtection is the AWS-level backstop (T2/(c)): with it on, the<br />cluster cannot be deleted even by an authorized principal until it is<br />cleared. Defaults on. | true | Optional: \{\} <br /> |


#### Tenant



Tenant is the cluster-scoped organizational owner of one or more
Platforms. Provides aggregate budget / readiness / suspension views and
a single point for non-technical persona dashboards to land on.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `platform.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `Tenant` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[TenantSpec](#tenantspec)_ |  |  |  |
| `status` _[TenantStatus](#tenantstatus)_ |  |  |  |


#### TenantSpec



TenantSpec describes an organization (or sub-org) that owns one or more
Platforms. Tenant is cluster-scoped — it doesn't represent a Kubernetes
namespace; it represents an organizational boundary that crosses
Platforms. The relationship to Platform is by `Platform.spec.tenant`
referencing `Tenant.metadata.name`.



_Appears in:_
- [Tenant](#tenant)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `displayName` _string_ | DisplayName is the human-readable tenant name shown in dashboards<br />and persona UX. |  | Optional: \{\} <br /> |
| `primaryPersona` _string_ | PrimaryPersona drives default values for new Platforms onboarded<br />into this tenant. One of the standard persona names. | generic | Enum: [sales-ops support finance ops founder eng marketing legal generic] <br /> |
| `contact` _[ContactSpec](#contactspec)_ | Contact carries human-readable owner info (Slack channel, on-call<br />rotation, billing email) for ops to reach. |  | Optional: \{\} <br /> |
| `compliance` _[ComplianceSpec](#compliancespec)_ | Compliance is the posture expected of every Platform this Tenant owns. A<br />Platform may declare more than its Tenant, never less: `cloudgov platform<br />audit` reports a Platform declaring less than its Tenant as a finding.<br />Nothing copies this value down — each Platform declares its own. |  | Optional: \{\} <br /> |
| `aggregateMonthlyBudgetUsd` _string_ | AggregateMonthlyBudgetUsd is the soft cap on the SUM of all owned<br />Platforms' BudgetPolicy.spec.monthlyUsd. Status reports whether the<br />sum exceeds this; the operator does not enforce — each Platform's<br />own BudgetPolicy is the enforcement layer. Modeled as a decimal-<br />string to mirror BudgetPolicy.monthlyUsd. |  | Pattern: `^[0-9]+(\.[0-9]\{1,2\})?$` <br />Optional: \{\} <br /> |


#### TenantStatus



TenantStatus aggregates the state of Platforms owned by this Tenant.



_Appears in:_
- [Tenant](#tenant)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Active, Suspended (any owned Platform suspended),<br />Failed. |  | Optional: \{\} <br /> |
| `platformCount` _integer_ | PlatformCount is the number of Platform CRs whose<br />spec.tenant == Tenant.metadata.name. |  | Optional: \{\} <br /> |
| `readyPlatformCount` _integer_ | ReadyPlatformCount is the subset of PlatformCount in phase=Ready. |  | Optional: \{\} <br /> |
| `suspendedPlatformCount` _integer_ | SuspendedPlatformCount is the subset in phase=Suspended. |  | Optional: \{\} <br /> |
| `aggregateSpendUsd` _string_ | AggregateSpendUsd is the sum of CurrentSpendUsd across all owned<br />BudgetPolicies (one per owned Platform). |  | Optional: \{\} <br /> |
| `aggregateBudgetUsd` _string_ | AggregateBudgetUsd is the sum of MonthlyUsd across all owned<br />BudgetPolicies. |  | Optional: \{\} <br /> |
| `percentOfBudget` _integer_ | PercentOfBudget — 0..200+. Computed from AggregateSpend /<br />AggregateBudget. When > 100 a TenantBudgetExceeded condition fires. |  | Optional: \{\} <br /> |
| `lastReconciled` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | LastReconciled timestamp. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


