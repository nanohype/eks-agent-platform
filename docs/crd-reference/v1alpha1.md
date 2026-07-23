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
- [BatchJob](#batchjob)
- [ModelGateway](#modelgateway)
- [SandboxPool](#sandboxpool)



#### AgentFleet



AgentFleet is a Platform-scoped composition of one or more agents on top
of upstream kagent CRs. The scale subresource is deliberately omitted:
`kubectl scale` would be ambiguous (min? max? per-agent?) for a fleet,
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



AgentFleetSpec composes kagent Agent / ModelConfig / ToolServer CRs plus
platform-specific scaffolding (KEDA, NetworkPolicy, IRSA binding).



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
IRSA ServiceAccount.



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
| `tools` _[ToolRef](#toolref) array_ | Tools is the list of kagent ToolServer references. |  | Optional: \{\} <br /> |
| `replicas` _integer_ | Replicas overrides the fleet-wide scaling minimum for this agent. |  | Optional: \{\} <br /> |


#### BatchJob



BatchJob runs a single Amazon Bedrock batch-inference job for a Platform tenant.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.nanohype.dev/v1alpha1` | | |
| `kind` _string_ | `BatchJob` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BatchJobSpec](#batchjobspec)_ |  |  |  |
| `status` _[BatchJobStatus](#batchjobstatus)_ |  |  |  |


#### BatchJobSpec



BatchJobSpec submits an Amazon Bedrock batch-inference job
(CreateModelInvocationJob): an S3 JSONL of records in, an S3 JSONL of
results out. One BatchJob maps to exactly one Bedrock job — there is no
schedule; create a new CR for each run (the reconciler is idempotent on
the spec, so re-applying the same CR never double-submits).



_Appears in:_
- [BatchJob](#batchjob)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `platformRef` _[LocalRef](#localref)_ |  |  |  |
| `modelId` _string_ | ModelID is the Bedrock model id or inference-profile id the batch job<br />invokes (e.g. "anthropic.claude-3-5-sonnet-20241022-v2:0" or a<br />cross-region "us.anthropic.…" profile). Validated server-side by<br />Bedrock; kept a free string here like Identity.AllowedModels. |  | MinLength: 1 <br /> |
| `modelInvocationType` _string_ | ModelInvocationType selects the record schema in the input JSONL —<br />raw InvokeModel bodies or Converse turns. | InvokeModel | Enum: [InvokeModel Converse] <br /> |
| `inputS3Uri` _string_ | InputS3Uri is the s3:// URI of the input JSONL (or its prefix). |  | Pattern: `^s3://.+` <br /> |
| `outputS3Prefix` _string_ | OutputS3Prefix is the s3:// prefix Bedrock writes results under. |  | Pattern: `^s3://.+` <br /> |
| `timeoutHours` _integer_ | TimeoutHours bounds the job's runtime. Bedrock requires 24..168. | 24 | Maximum: 168 <br />Minimum: 24 <br /> |
| `serviceRoleArnOverride` _string_ | ServiceRoleArnOverride replaces the operator-resolved Bedrock batch<br />service role (the role Bedrock assumes to read input / write output).<br />Normally empty — the reconciler injects the SSM-resolved role. |  | Optional: \{\} <br /> |


#### BatchJobStatus



BatchJobStatus tracks the Bedrock job the reconciler submitted and polls.



_Appears in:_
- [BatchJob](#batchjob)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `jobArn` _string_ | JobArn is the submitted job's ARN. Non-empty is the idempotency guard:<br />once set, the reconciler polls rather than re-submitting. |  | Optional: \{\} <br /> |
| `jobName` _string_ | JobName is the deterministic, Bedrock-sanitized job name. |  | Optional: \{\} <br /> |
| `phase` _string_ | Phase: Pending, Provisioning, Running, Succeeded, Failed, Stopped. |  | Optional: \{\} <br /> |
| `submittedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | SubmittedAt / CompletedAt timestamps. |  | Optional: \{\} <br /> |
| `completedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ |  |  | Optional: \{\} <br /> |
| `outputLocation` _string_ | OutputLocation is the s3:// URI Bedrock reports for the results. |  | Optional: \{\} <br /> |
| `recordCount` _integer_ | RecordCount / SucceededCount / FailedCount mirror Bedrock's<br />ProcessedRecordCount / SuccessRecordCount / ErrorRecordCount once the<br />job is running or terminal. |  | Optional: \{\} <br /> |
| `succeededCount` _integer_ |  |  | Optional: \{\} <br /> |
| `failedCount` _integer_ |  |  | Optional: \{\} <br /> |
| `message` _string_ | Message carries the last Bedrock status / failure reason. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


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



ModelGatewayStatus surfaces the agentgateway Route/Listener state.



_Appears in:_
- [ModelGateway](#modelgateway)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Provisioning, Ready, Failed. |  | Optional: \{\} <br /> |
| `endpoint` _string_ | Endpoint is the cluster-internal hostname of the gateway. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | ObservedGeneration is the last spec.generation reconciled. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |  |  | Optional: \{\} <br /> |


#### ModelRouteSpec



ModelRouteSpec is a single named route.



_Appears in:_
- [ModelGatewaySpec](#modelgatewayspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `modelFamily` _string_ | ModelFamily: anthropic \| meta \| mistral \| cohere \| amazon-titan \|<br />amazon-nova \| stability. |  | Enum: [anthropic meta mistral cohere amazon-titan amazon-nova stability] <br /> |
| `modelId` _string_ | ModelId is the canonical Bedrock model ID or inference profile ID. |  |  |
| `crossRegionProfile` _string_ | CrossRegionProfile enables a Bedrock cross-region inference profile. |  | Optional: \{\} <br /> |
| `rateLimit` _integer_ | RateLimit caps requests per minute (not tokens) on this route. The<br />operator renders it into an agentgateway local rate-limit policy with<br />unit=Minutes; 0 or unset disables rate limiting for the route. |  | Optional: \{\} <br /> |
| `guardrailRef` _[LocalRef](#localref)_ | GuardrailRef overrides the gateway's default guardrail. |  | Optional: \{\} <br /> |


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
| `queueUrl` _string_ | QueueUrl is the SQS queue the fleet's work originates from. When<br />set the operator emits a KEDA aws-sqs-queue trigger; otherwise a<br />CPU-utilization placeholder. The tenant IRSA role must have<br />sqs:GetQueueAttributes on this queue (granted via the agent-iam<br />baseline policy + an in-policy resource ARN derived from the URL). |  | Pattern: `^https://sqs\.[a-z0-9-]+\.amazonaws\.com/[0-9]\{12\}/[A-Za-z0-9_-]+(\.fifo)?$` <br />Optional: \{\} <br /> |


#### ToolRef



ToolRef references a kagent ToolServer by name.



_Appears in:_
- [AgentSpec](#agentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |



## governance.nanohype.dev/v1alpha1

Package v1alpha1 contains API Schema definitions for the governance v1alpha1 API group.

### Resource Types
- [BudgetPolicy](#budgetpolicy)
- [EvalSuite](#evalsuite)



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
| `sessionRoleMaxDurationSeconds` _integer_ | SessionRoleMaxDurationSeconds caps the assumed session lifetime. Because<br />the caller is the tenant IRSA role, AWS STS role chaining hard-caps a<br />chained session at 3600s regardless of this value; larger values only<br />matter if the caller ever changes. Defaults to 3600. | 3600 | Maximum: 43200 <br />Minimum: 900 <br />Optional: \{\} <br /> |


#### BudgetRef



BudgetRef points at a BudgetPolicy by name.



_Appears in:_
- [PlatformSpec](#platformspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |


#### CacheConfig



CacheConfig tunes the ElastiCache cluster. Engine and node type are reported
on drift (a resize is disruptive); replica count converges.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `engine` _string_ | Engine of the cache. Valkey is the default going-forward OSS engine.<br />Drift: reported. | valkey | Enum: [valkey redis] <br />Optional: \{\} <br /> |
| `nodeType` _string_ | NodeType sizes each node. Drift: reported — a node-type change is a<br />disruptive resize, surfaced as a condition rather than force-applied. | cache.t4g.micro | Optional: \{\} <br /> |
| `replicas` _integer_ | Replicas is the number of read replicas per shard; 0 (default) is a<br />single-node cache for a young tenant. Drift: converged. | 0 | Maximum: 5 <br />Minimum: 0 <br />Optional: \{\} <br /> |


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
	                        plus an operator-minted <env>-<platform>-scheduler-invoke
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



ComplianceSpec enables stricter defaults.



_Appears in:_
- [PlatformSpec](#platformspec)
- [TenantSpec](#tenantspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hipaa` _boolean_ | HIPAA: object-lock compliance mode, no cross-region inference, PII detect<br />required on Guardrails. |  | Optional: \{\} <br /> |
| `soc2` _boolean_ | SOC2: invocation logging required, kill-switch enabled. |  | Optional: \{\} <br /> |


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



_Appears in:_
- [PlatformSpec](#platformspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name identifies the datastore within its Platform and composes into the<br />AWS resource names (bucket, table, queue, cluster) alongside the env,<br />account, and platform tokens. A short RFC-1123 label; the tenant-substrate<br />module re-proves the exact composed length at its variable boundary, where<br />the env and account values are known. |  | Pattern: `^[a-z0-9]([a-z0-9-]\{0,16\}[a-z0-9])?$` <br /> |
| `kind` _[DatastoreKind](#datastorekind)_ | Kind selects the AWS implementation. Immutable — changing a live<br />datastore's kind would strand the provisioned resource. |  | Enum: [relational keyValue objectStore queue cache stream] <br /> |
| `deletionPolicy` _string_ | DeletionPolicy governs the underlying AWS resource when this datastore is<br />removed from spec or the Platform is deleted (T2).<br />  Retain (default): the resource is orphaned, tagged<br />    platform.nanohype.dev/owned-by and platform.nanohype.dev/released-at,<br />    so a `kubectl delete platform` never takes the data with it.<br />  Delete: the resource is torn down with the declaration.<br />Independent of the per-kind deletion_protection backstop, which defaults on<br />for relational and cache — two gates, both defaulting closed. | Retain | Enum: [Retain Delete] <br />Optional: \{\} <br /> |
| `relational` _[RelationalConfig](#relationalconfig)_ | Relational config; honored only when kind=relational. |  | Optional: \{\} <br /> |
| `keyValue` _[KeyValueConfig](#keyvalueconfig)_ | KeyValue config; honored only when kind=keyValue. |  | Optional: \{\} <br /> |
| `objectStore` _[ObjectStoreConfig](#objectstoreconfig)_ | ObjectStore config; honored only when kind=objectStore. |  | Optional: \{\} <br /> |
| `queue` _[QueueConfig](#queueconfig)_ | Queue config; honored only when kind=queue. |  | Optional: \{\} <br /> |
| `cache` _[CacheConfig](#cacheconfig)_ | Cache config; honored only when kind=cache. |  | Optional: \{\} <br /> |


#### DatastoreStatus



DatastoreStatus reports one datastore's observed state (T3/(a)). It lives
under PlatformStatus.Datastores, separate from the top-level Phase so a
still-creating datastore does not hold back the tenant's Ready (T6).



_Appears in:_
- [PlatformStatus](#platformstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name matches spec.datastores[].name. |  |  |
| `kind` _[DatastoreKind](#datastorekind)_ | Kind echoes the declared kind. |  | Enum: [relational keyValue objectStore queue cache stream] <br />Optional: \{\} <br /> |
| `phase` _string_ | Phase: Pending, Provisioning, Ready, Drifted, Failed. |  | Optional: \{\} <br /> |
| `endpoint` _string_ | Endpoint is the connection address once available — Aurora/cache endpoint,<br />SQS queue URL, S3 bucket name, or MSK bootstrap brokers. |  | Optional: \{\} <br /> |
| `arn` _string_ | ARN of the provisioned resource. |  | Optional: \{\} <br /> |
| `secretName` _string_ | SecretName is the resolved name of the credentials secret the datastore<br />publishes — the RDS-managed master secret for relational — so the tenant<br />chart reads one predictable place instead of hand-wiring it per app (T7). |  | Optional: \{\} <br /> |
| `drift` _string array_ | Drift lists spec fields observed to differ from AWS that the operator<br />reports but does not converge (the destructive-to-correct fields per T3).<br />Empty when in sync. |  | Optional: \{\} <br /> |


#### GlobalSecondaryIndex



GlobalSecondaryIndex declares a DynamoDB GSI. The key schema is immutable
(AWS recreates the index to change it); drift on projection is reported.



_Appears in:_
- [KeyValueConfig](#keyvalueconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the index. |  | Pattern: `^[a-zA-Z0-9_.-]\{3,255\}$` <br /> |
| `partitionKey` _[AttributeSchema](#attributeschema)_ | PartitionKey (hash key) of the index. |  |  |
| `sortKey` _[AttributeSchema](#attributeschema)_ | SortKey (range key) of the index. |  | Optional: \{\} <br /> |
| `projection` _string_ | Projection controls which attributes are copied into the index. | ALL | Enum: [ALL KEYS_ONLY INCLUDE] <br />Optional: \{\} <br /> |


#### IdentitySpec



IdentitySpec wires the per-Platform IRSA role. The controller reconciles a
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
| `extraPolicyArns` _string array_ | ExtraPolicyArns are managed IAM policies attached on top of the baseline. |  | Optional: \{\} <br /> |
| `capabilities` _[Capability](#capability) array_ | Capabilities are managed AWS capabilities outside the datastore vocabulary<br />(SES send, EventBridge Scheduler). Each drives an operator-generated<br />`capability-access` inline policy — and, for eventBridgeScheduler, a minted<br />scheduler-invoke role — so a tenant declares what it needs rather than<br />referencing a hand-written managed policy through extraPolicyArns. |  | Enum: [ses eventBridgeScheduler] <br />MaxItems: 8 <br />Optional: \{\} <br /> |
| `directSecretReads` _string array_ | DirectSecretReads names the application secrets this Platform's pods read<br />directly through the pod role via the AWS SDK, each a name under the<br />tenant's own <platform>/<env>/ prefix in Secrets Manager (e.g.<br />"grafana/oncall-webhook-hmac"). The controller grants<br />secretsmanager:GetSecretValue/DescribeSecret on exactly those secrets in<br />the tenant-secrets inline policy — no prefix wildcard. Secret material<br />projected into the pod by the chart's ExternalSecret is resolved by the<br />External Secrets controller's own identity and needs no entry here;<br />leaving this empty means the tenant role holds no Secrets Manager grant. |  | MaxItems: 16 <br />items:MaxLength: 256 <br />items:Pattern: `^[A-Za-z0-9][A-Za-z0-9/_+=.@-]*$` <br />Optional: \{\} <br /> |


#### KeyValueConfig



KeyValueConfig tunes the DynamoDB table. The key schema is immutable; billing
mode, TTL, and point-in-time recovery converge on drift.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `partitionKey` _[AttributeSchema](#attributeschema)_ | PartitionKey (hash key). Immutable after create. |  |  |
| `sortKey` _[AttributeSchema](#attributeschema)_ | SortKey (range key). Immutable after create. |  | Optional: \{\} <br /> |
| `billingMode` _string_ | BillingMode. PAY_PER_REQUEST (default) suits a young tenant with unknown<br />traffic; PROVISIONED is for steady, predictable load. Drift: converged. | PAY_PER_REQUEST | Enum: [PAY_PER_REQUEST PROVISIONED] <br />Optional: \{\} <br /> |
| `ttlAttribute` _string_ | TTLAttribute names the item attribute holding an epoch expiry; empty<br />disables TTL. Drift: converged. |  | Optional: \{\} <br /> |
| `pointInTimeRecovery` _boolean_ | PointInTimeRecovery enables continuous backups. Defaults on. Drift:<br />converged. | true | Optional: \{\} <br /> |
| `globalSecondaryIndexes` _[GlobalSecondaryIndex](#globalsecondaryindex) array_ | GlobalSecondaryIndexes declared on the table. |  | Optional: \{\} <br /> |


#### ObjectStoreConfig



ObjectStoreConfig tunes the S3 bucket. Encryption and public-access blocking
are always on and not configurable. Both fields converge on drift.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `versioning` _boolean_ | Versioning keeps prior object versions. Defaults on; set false only for a<br />bucket of regenerable data where prior versions add cost with no recovery<br />value. Drift: converged. | true | Optional: \{\} <br /> |
| `lifecycleExpireDays` _integer_ | LifecycleExpireDays expires objects after N days; 0 (default) keeps them<br />indefinitely. Drift: converged. | 0 | Minimum: 0 <br />Optional: \{\} <br /> |


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
| `identity` _[IdentitySpec](#identityspec)_ | Identity controls how the IRSA role is named + which Bedrock models are<br />reachable. |  |  |
| `compliance` _[ComplianceSpec](#compliancespec)_ | Compliance flags drive stricter defaults across the Platform. |  | Optional: \{\} <br /> |
| `isolation` _string_ | Isolation is the workload-isolation tier:<br />  - namespace (default): namespace RBAC + default-deny NetworkPolicy +<br />    ResourceQuota + PSS-restricted, tenant workloads on the host API server.<br />  - vcluster: the same host-side containment PLUS a per-Platform virtual<br />    cluster, so tenant code that talks to the Kubernetes API talks to its own<br />    API server, not the host's (API-server-level isolation — NOT kernel/node<br />    isolation; see docs/adr/0009-vcluster-isolation-tier.md and SECURITY.md).<br />Immutable: switching tiers on a live Platform is a migration (it would strand<br />the virtual cluster and its synced host objects), so the tier is fixed at<br />create time. Re-declare the Platform to change it. Enforced at admission by<br />the CEL transition rule below — an invalid tier flip fails the apply rather<br />than silently half-reconciling. | namespace | Enum: [namespace vcluster] <br />Optional: \{\} <br /> |
| `attribution` _[AttributionSpec](#attributionspec)_ | Attribution opts the Platform into per-session human attribution. When<br />set, the operator provisions a session role — assumable by the tenant<br />IRSA role with the operator carried as STS SourceIdentity, scoped to the<br />tenant baseline (Bedrock invoke) and NOT broad sts:AssumeRole — plus a<br />ClusterRole letting the tenant ServiceAccount impersonate the named<br />operators at the apiserver. fab's role-session entrypoint consumes both,<br />so an agent's AWS + Kubernetes actions attribute to a named human.<br />nil = unattributed (the default). |  | Optional: \{\} <br /> |
| `datastores` _[DatastoreSpec](#datastorespec) array_ | Datastores declares the tenant's stateful substrate — the databases,<br />buckets, queues, caches, and streams it needs. Each entry is a declaration,<br />not a hand-written component: the tenant-substrate tofu module provisions<br />the heavy resource from this same list and the operator generates the<br />scoped IAM policy that reaches it, so adding a tenant never means authoring<br />a landing-zone component. Empty for a Platform with no stateful needs. |  | Optional: \{\} <br /> |


#### PlatformStatus



PlatformStatus captures the controller's view of the world.



_Appears in:_
- [Platform](#platform)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase: Pending, Provisioning, Ready, Suspended, Failed. |  | Optional: \{\} <br /> |
| `iamRoleArn` _string_ | IamRoleArn is the per-Platform IRSA role created by the controller. |  | Optional: \{\} <br /> |
| `sessionRoleArn` _string_ | SessionRoleArn is the per-Platform attribution session role, created when<br />spec.attribution is set. Empty when attribution is off. |  | Optional: \{\} <br /> |
| `namespace` _string_ | Namespace is the tenant namespace the controller provisioned. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | ObservedGeneration is the last spec.generation the controller reconciled. |  | Optional: \{\} <br /> |
| `suspendedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_ | SuspendedAt is the timestamp at which the kill-switch fired. When<br />non-nil the operator stops reattaching the baseline IAM policy and<br />the AgentFleetReconciler scales fleets to zero. Resets to nil only<br />when ops clears the iam:TagRole 'platform.nanohype.dev/suspended'<br />marker on the tenant IRSA role. |  | Optional: \{\} <br /> |
| `suspendedReason` _string_ | SuspendedReason carries the kill-switch's reason (e.g.<br />'budget-exceeded'). Same lifecycle as SuspendedAt. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ | Conditions follows the standard kubernetes pattern. |  | Optional: \{\} <br /> |
| `datastores` _[DatastoreStatus](#datastorestatus) array_ | Datastores reports per-datastore observed state, separate from the<br />top-level Phase: a Platform is Ready once its namespace, quota, and<br />identity are live, while each datastore reports its own readiness here so a<br />still-creating Aurora cluster does not gate the tenant's Ready (T6). |  | Optional: \{\} <br /> |


#### QueueConfig



QueueConfig tunes the SQS queue. FIFO-ness is immutable (a FIFO and a standard
queue are different resources); the remaining fields converge on drift.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `fifo` _boolean_ | FIFO makes an exactly-once, ordered queue. Immutable after create. | false | Optional: \{\} <br /> |
| `visibilityTimeoutSeconds` _integer_ | VisibilityTimeoutSeconds before a received-but-unacked message is<br />redelivered. Drift: converged. | 30 | Maximum: 43200 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `messageRetentionSeconds` _integer_ | MessageRetentionSeconds a message is kept before it expires (default 4<br />days). Drift: converged. | 345600 | Maximum: 1.2096e+06 <br />Minimum: 60 <br />Optional: \{\} <br /> |
| `maxReceiveCount` _integer_ | MaxReceiveCount, when > 0, provisions a dead-letter queue and redrives a<br />message to it after this many failed receives; 0 (default) means no DLQ.<br />Drift: converged. | 0 | Maximum: 1000 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### RelationalConfig



RelationalConfig tunes the Aurora PostgreSQL Serverless v2 cluster. Omitting
the block provisions the young/light default: 0.5–8 ACU, 7-day backups,
deletion protection on.



_Appears in:_
- [DatastoreSpec](#datastorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `engineVersion` _string_ | EngineVersion of Aurora PostgreSQL. Drift: reported, never converged — an<br />out-of-band engine change is not force-corrected because a downgrade is<br />destructive; the operator raises a Drifted condition instead. | 16.6 | Optional: \{\} <br /> |
| `minACU` _string_ | MinACU is the Serverless v2 floor in Aurora Capacity Units, in 0.5-ACU<br />steps (e.g. "0.5", "1", "8"). Serialized as a string, per the Kubernetes<br />convention for fractional values. The exact 0.5–256 range and the<br />maxACU >= minACU relation are enforced at the tenant-substrate module's<br />variable boundary. Drift: converged — the operator resets scaling bounds<br />to spec. | 0.5 | Pattern: `^([1-9][0-9]\{0,2\}(\.5)?\|0\.5)$` <br />Optional: \{\} <br /> |
| `maxACU` _string_ | MaxACU is the Serverless v2 ceiling, in 0.5-ACU steps. Drift: converged. | 8 | Pattern: `^([1-9][0-9]\{0,2\}(\.5)?\|0\.5)$` <br />Optional: \{\} <br /> |
| `backupRetentionDays` _integer_ | BackupRetentionDays for automated backups. Drift: converged. | 7 | Maximum: 35 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `deletionProtection` _boolean_ | DeletionProtection is the AWS-level backstop (T2/(c)): with it on, the<br />cluster cannot be deleted even by an authorized principal until it is<br />cleared. Defaults on. Drift: converged. | true | Optional: \{\} <br /> |


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
| `compliance` _[ComplianceSpec](#compliancespec)_ | Compliance baseline applied to every Platform owned by this Tenant<br />unless the Platform itself sets a stricter value. |  | Optional: \{\} <br /> |
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


