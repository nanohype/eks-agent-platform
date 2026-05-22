# API Reference

## Packages

- [agents.stxkxs.io/v1alpha1](#agentsstxkxsiov1alpha1)

## agents.stxkxs.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group.

### Resource Types

- [AgentFleet](#agentfleet)
- [BudgetPolicy](#budgetpolicy)
- [EvalSuite](#evalsuite)
- [ModelGateway](#modelgateway)
- [Platform](#platform)
- [SandboxPool](#sandboxpool)
- [Tenant](#tenant)

#### AgentFleet

AgentFleet is a Platform-scoped composition of one or more agents on top
of upstream kagent CRs. The scale subresource is deliberately omitted:
`kubectl scale` would be ambiguous (min? max? per-agent?) for a fleet,
so per-agent replica overrides live on AgentSpec.Replicas and fleet-wide
behavior is driven by .spec.scaling (KEDA) instead.

| Field                                                                                                              | Description                                                     | Default | Validation |
| ------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                              | `agents.stxkxs.io/v1alpha1`                                     |         |            |
| `kind` _string_                                                                                                    | `AgentFleet`                                                    |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |         |            |
| `spec` _[AgentFleetSpec](#agentfleetspec)_                                                                         |                                                                 |         |            |
| `status` _[AgentFleetStatus](#agentfleetstatus)_                                                                   |                                                                 |         |            |

#### AgentFleetSpec

AgentFleetSpec composes kagent Agent / ModelConfig / ToolServer CRs plus
platform-specific scaffolding (KEDA, NetworkPolicy, IRSA binding).

_Appears in:_

- [AgentFleet](#agentfleet)

| Field                                    | Description                                                       | Default | Validation            |
| ---------------------------------------- | ----------------------------------------------------------------- | ------- | --------------------- |
| `platformRef` _[LocalRef](#localref)_    |                                                                   |         |                       |
| `agents` _[AgentSpec](#agentspec) array_ | Agents is the list of agents to provision in this fleet.          |         | MinItems: 1 <br />    |
| `scaling` _[ScalingSpec](#scalingspec)_  | Scaling controls KEDA's ScaledObject for the runtime Deployments. |         | Optional: \{\} <br /> |
| `compute` _[ComputeSpec](#computespec)_  | Compute optionally requests an AcceleratorClaim.                  |         | Optional: \{\} <br /> |

#### AgentFleetStatus

AgentFleetStatus reports rollout state.

_Appears in:_

- [AgentFleet](#agentfleet)

| Field                                                                                                                    | Description                                                     | Default | Validation            |
| ------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | --------------------- |
| `phase` _string_                                                                                                         | Phase: Pending, Provisioning, Ready, ScaledToZero, Failed.      |         | Optional: \{\} <br /> |
| `readyAgents` _integer_                                                                                                  | ReadyAgents counts agents whose downstream Deployment is ready. |         | Optional: \{\} <br /> |
| `observedGeneration` _integer_                                                                                           | ObservedGeneration is the last spec.generation reconciled.      |         | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |                                                                 |         | Optional: \{\} <br /> |

#### AgentSpec

AgentSpec is one agent in the fleet.

_Appears in:_

- [AgentFleetSpec](#agentfleetspec)

| Field                               | Description                                                       | Default | Validation            |
| ----------------------------------- | ----------------------------------------------------------------- | ------- | --------------------- |
| `name` _string_                     |                                                                   |         |                       |
| `systemPrompt` _string_             | SystemPrompt is the agent's instruction text.                     |         |                       |
| `modelRoute` _string_               | ModelRoute is the named route on the Platform's ModelGateway.     |         |                       |
| `tools` _[ToolRef](#toolref) array_ | Tools is the list of kagent ToolServer references.                |         | Optional: \{\} <br /> |
| `replicas` _integer_                | Replicas overrides the fleet-wide scaling minimum for this agent. |         | Optional: \{\} <br /> |

#### BudgetPolicy

BudgetPolicy caps monthly spend per Platform and triggers the kill-switch at 120% of the threshold.

| Field                                                                                                              | Description                                                     | Default | Validation |
| ------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                              | `agents.stxkxs.io/v1alpha1`                                     |         |            |
| `kind` _string_                                                                                                    | `BudgetPolicy`                                                  |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |         |            |
| `spec` _[BudgetPolicySpec](#budgetpolicyspec)_                                                                     |                                                                 |         |            |
| `status` _[BudgetPolicyStatus](#budgetpolicystatus)_                                                               |                                                                 |         |            |

#### BudgetPolicySpec

BudgetPolicySpec sets monthly spend caps per Platform.

_Appears in:_

- [BudgetPolicy](#budgetpolicy)

| Field                                    | Description                                                                                                                                                                                                                                                                                                                                                                             | Default     | Validation                                                     |
| ---------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------- | -------------------------------------------------------------- |
| `platformRef` _[LocalRef](#localref)_    |                                                                                                                                                                                                                                                                                                                                                                                         |             |                                                                |
| `monthlyUsd` _string_                    | MonthlyUsd is the soft threshold expressed as a decimal-string USD amount<br />(e.g. "2500", "1500.50"). KillSwitch fires at 120% of this. Modeled as<br />string for symmetry with Status.CurrentSpendUsd and so future v1 can<br />support fractional cents without a lossy int32 → string conversion. The<br />pattern enforces non-negative decimal with optional 2-digit fraction. |             | MinLength: 1 <br />Pattern: `^[0-9]+(\.[0-9]\{1,2\})?$` <br /> |
| `alertThresholdsPercent` _integer array_ | AlertThresholdsPercent — fire WarnEvent at these % of the threshold.                                                                                                                                                                                                                                                                                                                    | [50 80 100] | Optional: \{\} <br />                                          |
| `killSwitchEnabled` _boolean_            | KillSwitchEnabled — when false, breach at 120% is logged but not acted on.<br />Use sparingly; SOC2 platforms must keep this true.                                                                                                                                                                                                                                                      | true        |                                                                |

#### BudgetPolicyStatus

BudgetPolicyStatus surfaces the latest spend reading. The budget reconciler
updates this on every tick (hourly in prod, 5m in dev) with current spend,
percent-of-budget, the alert thresholds crossed, and reconcile conditions.

_Appears in:_

- [BudgetPolicy](#budgetpolicy)

| Field                                                                                                                    | Description                                                                                         | Default | Validation            |
| ------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------- | ------- | --------------------- |
| `currentSpendUsd` _string_                                                                                               | CurrentSpendUsd is the most recent spend snapshot.                                                  |         | Optional: \{\} <br /> |
| `percentOfBudget` _integer_                                                                                              | PercentOfBudget — 0..200+.                                                                          |         | Optional: \{\} <br /> |
| `lastReconciled` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_             | LastReconciled timestamp.                                                                           |         | Optional: \{\} <br /> |
| `killSwitchFiredAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_          | KillSwitchFiredAt — non-null if the kill-switch fired and the platform<br />is currently suspended. |         | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |                                                                                                     |         | Optional: \{\} <br /> |

#### BudgetRef

BudgetRef points at a BudgetPolicy by name.

_Appears in:_

- [PlatformSpec](#platformspec)

| Field           | Description | Default | Validation |
| --------------- | ----------- | ------- | ---------- |
| `name` _string_ |             |         |            |

#### ComplianceSpec

ComplianceSpec enables stricter defaults.

_Appears in:_

- [PlatformSpec](#platformspec)
- [TenantSpec](#tenantspec)

| Field             | Description                                                                                            | Default | Validation            |
| ----------------- | ------------------------------------------------------------------------------------------------------ | ------- | --------------------- |
| `hipaa` _boolean_ | HIPAA: object-lock compliance mode, no cross-region inference, PII detect<br />required on Guardrails. |         | Optional: \{\} <br /> |
| `soc2` _boolean_  | SOC2: invocation logging required, kill-switch enabled.                                                |         | Optional: \{\} <br /> |

#### ComputeSpec

ComputeSpec requests accelerator resources via DRA.

_Appears in:_

- [AgentFleetSpec](#agentfleetspec)

| Field                                                                                                                                   | Description                                                                                                                                    | Default | Validation            |
| --------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- | ------- | --------------------- |
| `acceleratorClaim` _[LocalRef](#localref)_                                                                                              | AcceleratorClaim references an AcceleratorClaim CR. The operator<br />translates that into a ResourceClaimTemplate referenced in the pod spec. |         |                       |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_ | Resources are pod resource requests/limits.                                                                                                    |         | Optional: \{\} <br /> |

#### ContactSpec

ContactSpec carries owner / on-call / billing reach paths.

_Appears in:_

- [TenantSpec](#tenantspec)

| Field                     | Description                                                    | Default | Validation            |
| ------------------------- | -------------------------------------------------------------- | ------- | --------------------- |
| `slackChannel` _string_   | SlackChannel for tenant-wide notifications (e.g. "#acme-ops"). |         | Optional: \{\} <br /> |
| `oncallRotation` _string_ | OncallRotation — Pagerduty schedule key or similar identifier. |         | Optional: \{\} <br /> |
| `billingEmail` _string_   | BillingEmail — invoice + budget-breach notification recipient. |         | Optional: \{\} <br /> |

#### EvalCase

EvalCase is a single test case.

_Appears in:_

- [EvalSuiteSpec](#evalsuitespec)

| Field                           | Description | Default | Validation |
| ------------------------------- | ----------- | ------- | ---------- |
| `name` _string_                 |             |         |            |
| `input` _string_                |             |         |            |
| `expectContains` _string array_ |             |         |            |
| `maxLatencyMs` _integer_        |             |         |            |
| `maxCostUsd` _string_           |             |         |            |

#### EvalSuite

EvalSuite is a scheduled evaluation run against an AgentFleet's agents.

| Field                                                                                                              | Description                                                     | Default | Validation |
| ------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                              | `agents.stxkxs.io/v1alpha1`                                     |         |            |
| `kind` _string_                                                                                                    | `EvalSuite`                                                     |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |         |            |
| `spec` _[EvalSuiteSpec](#evalsuitespec)_                                                                           |                                                                 |         |            |
| `status` _[EvalSuiteStatus](#evalsuitestatus)_                                                                     |                                                                 |         |            |

#### EvalSuiteSpec

EvalSuiteSpec defines a periodic evaluation run against an AgentFleet.

_Appears in:_

- [EvalSuite](#evalsuite)

| Field                                   | Description                                                                                                                                                                                                                                                                                       | Default | Validation                                   |
| --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | -------------------------------------------- |
| `platformRef` _[LocalRef](#localref)_   |                                                                                                                                                                                                                                                                                                   |         |                                              |
| `agentFleetRef` _[LocalRef](#localref)_ | AgentFleetRef targets the fleet whose agents are under test.                                                                                                                                                                                                                                      |         |                                              |
| `schedule` _string_                     | Schedule (cron) — when to run the suite. Empty = manual only.                                                                                                                                                                                                                                     |         | Optional: \{\} <br />                        |
| `cases` _[EvalCase](#evalcase) array_   | Cases is the list of test cases (input prompt + expected criteria).<br />In production these are typically loaded from an S3 manifest; this<br />inline list is for small / dev suites.                                                                                                           |         | Optional: \{\} <br />                        |
| `casesFromManifest` _string_            | CasesFromManifest loads from `eval-reports/<platform>/manifests/<name>.json`<br />in the eval-reports S3 bucket.                                                                                                                                                                                  |         | Optional: \{\} <br />                        |
| `passThreshold` _string_                | PassThreshold (0..1) is the required mean score for the run to be<br />marked passing. Argo Rollouts AnalysisTemplate consumes this signal.<br />Modeled as a string so reviewers see decimals in `kubectl get -o yaml`<br />without int<->float coercion surprises; pattern enforces 0.0 .. 1.0. | 0.85    | Pattern: `^(0(\.[0-9]+)?\|1(\.0+)?)$` <br /> |

#### EvalSuiteStatus

EvalSuiteStatus reports the latest run.

_Appears in:_

- [EvalSuite](#evalsuite)

| Field                                                                                                                    | Description                                            | Default | Validation            |
| ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------ | ------- | --------------------- |
| `lastRunAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_                  | LastRunAt timestamp.                                   |         | Optional: \{\} <br /> |
| `lastScore` _string_                                                                                                     | LastScore (mean across cases, 0..1).                   |         | Optional: \{\} <br /> |
| `lastReportUrl` _string_                                                                                                 | LastReportURL (s3:// URL to the rendered HTML report). |         | Optional: \{\} <br /> |
| `phase` _string_                                                                                                         | Phase: Pending, Running, Passed, Failed.               |         | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |                                                        |         | Optional: \{\} <br /> |

#### IdentitySpec

IdentitySpec wires the per-Platform IRSA role.

_Appears in:_

- [PlatformSpec](#platformspec)

| Field                                 | Description                                                                                                                                            | Default | Validation            |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | ------- | --------------------- |
| `allowedModels` _string array_        | AllowedModels is the list of Bedrock model IDs (or inference-profile IDs)<br />the IRSA role can invoke. Mutually exclusive with AllowedModelFamilies. |         | Optional: \{\} <br /> |
| `allowedModelFamilies` _string array_ | AllowedModelFamilies (e.g. ["anthropic", "meta", "amazon-nova"]) is<br />expanded by the controller into ARNs at reconcile time.                       |         | Optional: \{\} <br /> |
| `extraPolicyArns` _string array_      | ExtraPolicyArns are managed IAM policies attached on top of the baseline.                                                                              |         | Optional: \{\} <br /> |

#### LocalRef

LocalRef references a CR by name in the same namespace.

_Appears in:_

- [AgentFleetSpec](#agentfleetspec)
- [BudgetPolicySpec](#budgetpolicyspec)
- [ComputeSpec](#computespec)
- [EvalSuiteSpec](#evalsuitespec)
- [ModelGatewaySpec](#modelgatewayspec)
- [ModelRouteSpec](#modelroutespec)
- [SandboxPoolSpec](#sandboxpoolspec)

| Field           | Description | Default | Validation |
| --------------- | ----------- | ------- | ---------- |
| `name` _string_ |             |         |            |

#### ModelGateway

ModelGateway is a per-Platform gateway CR that fronts Bedrock for one or more named routes.

| Field                                                                                                              | Description                                                     | Default | Validation |
| ------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                              | `agents.stxkxs.io/v1alpha1`                                     |         |            |
| `kind` _string_                                                                                                    | `ModelGateway`                                                  |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |         |            |
| `spec` _[ModelGatewaySpec](#modelgatewayspec)_                                                                     |                                                                 |         |            |
| `status` _[ModelGatewayStatus](#modelgatewaystatus)_                                                               |                                                                 |         |            |

#### ModelGatewaySpec

ModelGatewaySpec configures a per-Platform gateway: the routes exposed,
which Bedrock models back them, and which Guardrail attaches.

_Appears in:_

- [ModelGateway](#modelgateway)

| Field                                              | Description                                                        | Default | Validation            |
| -------------------------------------------------- | ------------------------------------------------------------------ | ------- | --------------------- |
| `platformRef` _[LocalRef](#localref)_              | PlatformRef is the owning Platform.                                |         |                       |
| `routes` _[ModelRouteSpec](#modelroutespec) array_ | Routes is the list of named routes the gateway exposes.            |         | MinItems: 1 <br />    |
| `defaultGuardrailRef` _[LocalRef](#localref)_      | DefaultGuardrailRef applies when a Route does not specify its own. |         | Optional: \{\} <br /> |

#### ModelGatewayStatus

ModelGatewayStatus surfaces the agentgateway Route/Listener state.

_Appears in:_

- [ModelGateway](#modelgateway)

| Field                                                                                                                    | Description                                                | Default | Validation            |
| ------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------- | ------- | --------------------- |
| `phase` _string_                                                                                                         | Phase: Pending, Provisioning, Ready, Failed.               |         | Optional: \{\} <br /> |
| `endpoint` _string_                                                                                                      | Endpoint is the cluster-internal hostname of the gateway.  |         | Optional: \{\} <br /> |
| `observedGeneration` _integer_                                                                                           | ObservedGeneration is the last spec.generation reconciled. |         | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |                                                            |         | Optional: \{\} <br /> |

#### ModelRouteSpec

ModelRouteSpec is a single named route.

_Appears in:_

- [ModelGatewaySpec](#modelgatewayspec)

| Field                                  | Description                                                                                           | Default | Validation                                                                      |
| -------------------------------------- | ----------------------------------------------------------------------------------------------------- | ------- | ------------------------------------------------------------------------------- |
| `name` _string_                        |                                                                                                       |         |                                                                                 |
| `modelFamily` _string_                 | ModelFamily: anthropic \| meta \| mistral \| cohere \| amazon-titan \|<br />amazon-nova \| stability. |         | Enum: [anthropic meta mistral cohere amazon-titan amazon-nova stability] <br /> |
| `modelId` _string_                     | ModelId is the canonical Bedrock model ID or inference profile ID.                                    |         |                                                                                 |
| `crossRegionProfile` _string_          | CrossRegionProfile enables a Bedrock cross-region inference profile.                                  |         | Optional: \{\} <br />                                                           |
| `rateLimit` _integer_                  | RateLimit (requests per minute) is enforced at the gateway.                                           |         | Optional: \{\} <br />                                                           |
| `guardrailRef` _[LocalRef](#localref)_ | GuardrailRef overrides the gateway's default guardrail.                                               |         | Optional: \{\} <br />                                                           |

#### Platform

Platform is the top-level tenancy CR. Namespaced so that BudgetPolicy,
ModelGateway, AgentFleet, and EvalSuite references resolve in the same
namespace by name. The operator provisions the tenant workload namespace
(tenants-<platform-name>) separately at reconcile time; the Platform CR
itself lives in whichever namespace the cluster admin places it (typically
a management namespace such as eks-agent-platform).

| Field                                                                                                              | Description                                                     | Default | Validation |
| ------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                              | `agents.stxkxs.io/v1alpha1`                                     |         |            |
| `kind` _string_                                                                                                    | `Platform`                                                      |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |         |            |
| `spec` _[PlatformSpec](#platformspec)_                                                                             |                                                                 |         |            |
| `status` _[PlatformStatus](#platformstatus)_                                                                       |                                                                 |         |            |

#### PlatformSpec

PlatformSpec defines the desired state of a Platform — a tenancy boundary
hosting one or more AgentFleets, with its own budget, identity, and
guardrails.

_Appears in:_

- [Platform](#platform)

| Field                                            | Description                                                                                                                                                              | Default   | Validation                                                                       |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------- | -------------------------------------------------------------------------------- |
| `displayName` _string_                           | DisplayName is a human-readable name for dashboards and CLI output.                                                                                                      |           | Optional: \{\} <br />                                                            |
| `persona` _string_                               | Persona drives default values for AgentFleet, ModelGateway, and<br />dashboards. One of: sales-ops, support, finance, ops, founder, eng,<br />marketing, legal, generic. | generic   | Enum: [sales-ops support finance ops founder eng marketing legal generic] <br /> |
| `tenant` _string_                                | Tenant is the owning Tenant CR (one Tenant can own multiple Platforms).                                                                                                  |           |                                                                                  |
| `budget` _[BudgetRef](#budgetref)_               | Budget references a BudgetPolicy CR in the same namespace.                                                                                                               |           |                                                                                  |
| `identity` _[IdentitySpec](#identityspec)_       | Identity controls how the IRSA role is named + which Bedrock models are<br />reachable.                                                                                  |           |                                                                                  |
| `compliance` _[ComplianceSpec](#compliancespec)_ | Compliance flags drive stricter defaults across the Platform.                                                                                                            |           | Optional: \{\} <br />                                                            |
| `isolation` _string_                             | Isolation: namespace (default) or vCluster (hard isolation).                                                                                                             | namespace | Enum: [namespace vcluster] <br />Optional: \{\} <br />                           |

#### PlatformStatus

PlatformStatus captures the controller's view of the world.

_Appears in:_

- [Platform](#platform)

| Field                                                                                                                    | Description                                                                                                                                                                                                                                                                                                              | Default | Validation            |
| ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------- | --------------------- |
| `phase` _string_                                                                                                         | Phase: Pending, Provisioning, Ready, Suspended, Failed.                                                                                                                                                                                                                                                                  |         | Optional: \{\} <br /> |
| `iamRoleArn` _string_                                                                                                    | IamRoleArn is the per-Platform IRSA role created by the controller.                                                                                                                                                                                                                                                      |         | Optional: \{\} <br /> |
| `namespace` _string_                                                                                                     | Namespace is the tenant namespace the controller provisioned.                                                                                                                                                                                                                                                            |         | Optional: \{\} <br /> |
| `observedGeneration` _integer_                                                                                           | ObservedGeneration is the last spec.generation the controller reconciled.                                                                                                                                                                                                                                                |         | Optional: \{\} <br /> |
| `suspendedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_                | SuspendedAt is the timestamp at which the kill-switch fired. When<br />non-nil the operator stops reattaching the baseline IAM policy and<br />the AgentFleetReconciler scales fleets to zero. Resets to nil only<br />when ops clears the iam:TagRole 'agents.stxkxs.io/suspended'<br />marker on the tenant IRSA role. |         | Optional: \{\} <br /> |
| `suspendedReason` _string_                                                                                               | SuspendedReason carries the kill-switch's reason (e.g.<br />'budget-exceeded'). Same lifecycle as SuspendedAt.                                                                                                                                                                                                           |         | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ | Conditions follows the standard kubernetes pattern.                                                                                                                                                                                                                                                                      |         | Optional: \{\} <br /> |

#### SandboxPool

SandboxPool is a Platform-scoped pool of Managed Agents self-hosted
sandbox workers. The reconciler runs them as a Deployment on the
dedicated, tainted sandbox node pool, locked down by a default-deny
NetworkPolicy.

| Field                                                                                                              | Description                                                     | Default | Validation |
| ------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                              | `agents.stxkxs.io/v1alpha1`                                     |         |            |
| `kind` _string_                                                                                                    | `SandboxPool`                                                   |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |         |            |
| `spec` _[SandboxPoolSpec](#sandboxpoolspec)_                                                                       |                                                                 |         |            |
| `status` _[SandboxPoolStatus](#sandboxpoolstatus)_                                                                 |                                                                 |         |            |

#### SandboxPoolSpec

SandboxPoolSpec declares a pool of Managed Agents self-hosted sandbox
workers for a `self_hosted` environment. The workers run Anthropic's
`ant beta:worker`, claiming sessions from the environment's work queue
and executing agent tool calls inside the cluster.

_Appears in:_

- [SandboxPool](#sandboxpool)

| Field                                                                                                                                        | Description                                                                                                                                                                                                      | Default | Validation            |
| -------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | --------------------- |
| `platformRef` _[LocalRef](#localref)_                                                                                                        | PlatformRef is the owning Platform. The pool's workers run in that<br />Platform's tenant namespace and the pool gates on Platform readiness.                                                                    |         |                       |
| `environmentId` _string_                                                                                                                     | EnvironmentID is the Managed Agents self*hosted environment whose<br />work queue these workers drain (an `env*...` id).                                                                                         |         |                       |
| `environmentKeySecret` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#secretkeyselector-v1-core)_ | EnvironmentKeySecret holds ANTHROPIC_ENVIRONMENT_KEY — the worker's<br />auth token, mounted into every worker pod.                                                                                              |         |                       |
| `apiKeySecret` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#secretkeyselector-v1-core)_         | APIKeySecret holds the organization API key. It is consumed only by<br />the work-queue autoscaler, never mounted into worker pods — Anthropic<br />warns the org key must not be reachable by agent tool calls. |         | Optional: \{\} <br /> |
| `image` _string_                                                                                                                             | Image overrides the sandbox worker image. Defaults to the platform's<br />published sandbox-worker image when empty.                                                                                             |         | Optional: \{\} <br /> |
| `scaling` _[SandboxScalingSpec](#sandboxscalingspec)_                                                                                        | Scaling bounds the worker count.                                                                                                                                                                                 |         | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#resourcerequirements-v1-core)_      | Resources are the per-worker-pod resource requests and limits.                                                                                                                                                   |         | Optional: \{\} <br /> |

#### SandboxPoolStatus

SandboxPoolStatus reports the pool's reconciled state.

_Appears in:_

- [SandboxPool](#sandboxpool)

| Field                                                                                                                    | Description                                                  | Default | Validation            |
| ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------ | ------- | --------------------- |
| `phase` _string_                                                                                                         | Phase: Pending, Ready, Suspended, Failed.                    |         | Optional: \{\} <br /> |
| `readyWorkers` _integer_                                                                                                 | ReadyWorkers is the worker Deployment's ready replica count. |         | Optional: \{\} <br /> |
| `observedGeneration` _integer_                                                                                           | ObservedGeneration is the last spec.generation reconciled.   |         | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |                                                              |         | Optional: \{\} <br /> |

#### SandboxScalingSpec

SandboxScalingSpec bounds the worker Deployment's replica count.

_Appears in:_

- [SandboxPoolSpec](#sandboxpoolspec)

| Field                        | Description                                                                                                                                 | Default | Validation                             |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- | ------- | -------------------------------------- |
| `minReplicas` _integer_      | MinReplicas is the worker-count floor. A pointer so 0 (scale to zero,<br />for the autoscaled path) is distinguishable from "field absent". | 1       | Minimum: 0 <br />Optional: \{\} <br /> |
| `maxReplicas` _integer_      | MaxReplicas is the worker-count ceiling for the autoscaler.                                                                                 | 10      | Minimum: 1 <br />Optional: \{\} <br /> |
| `queueDepthTarget` _integer_ | QueueDepthTarget is the work-queue depth per worker the autoscaler<br />aims for before adding workers.                                     | 5       | Minimum: 1 <br />Optional: \{\} <br /> |

#### ScalingSpec

ScalingSpec configures KEDA.

_Appears in:_

- [AgentFleetSpec](#agentfleetspec)

| Field                         | Description                                                                                                                                                                                                                                                                                                                                          | Default | Validation                                                                                                           |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------- |
| `enabled` _boolean_           | Enabled — when false, the operator scales the Deployment to 0 and<br />removes the ScaledObject. Toggled false by the kill-switch on budget<br />breach.                                                                                                                                                                                             | true    |                                                                                                                      |
| `min` _integer_               | Min replicas. Use a pointer so 0 (kill-switch state) is distinguishable<br />from "field absent" — with int32 + omitempty, the zero value gets<br />dropped and re-defaulted, making min=0 unrepresentable.                                                                                                                                          | 1       | Minimum: 0 <br />Optional: \{\} <br />                                                                               |
| `max` _integer_               | Max replicas.                                                                                                                                                                                                                                                                                                                                        | 10      | Minimum: 1 <br />Optional: \{\} <br />                                                                               |
| `queueDepthTrigger` _integer_ | QueueDepthTrigger: scale up when SQS depth exceeds this value.                                                                                                                                                                                                                                                                                       | 10      | Minimum: 1 <br />Optional: \{\} <br />                                                                               |
| `queueUrl` _string_           | QueueUrl is the SQS queue the fleet's work originates from. When<br />set the operator emits a KEDA aws-sqs-queue trigger; otherwise a<br />CPU-utilization placeholder. The tenant IRSA role must have<br />sqs:GetQueueAttributes on this queue (granted via the agent-iam<br />baseline policy + an in-policy resource ARN derived from the URL). |         | Pattern: `^https://sqs\.[a-z0-9-]+\.amazonaws\.com/[0-9]\{12\}/[A-Za-z0-9_-]+(\.fifo)?$` <br />Optional: \{\} <br /> |

#### Tenant

Tenant is the cluster-scoped organizational owner of one or more
Platforms. Provides aggregate budget / readiness / suspension views and
a single point for non-technical persona dashboards to land on.

| Field                                                                                                              | Description                                                     | Default | Validation |
| ------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                              | `agents.stxkxs.io/v1alpha1`                                     |         |            |
| `kind` _string_                                                                                                    | `Tenant`                                                        |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |         |            |
| `spec` _[TenantSpec](#tenantspec)_                                                                                 |                                                                 |         |            |
| `status` _[TenantStatus](#tenantstatus)_                                                                           |                                                                 |         |            |

#### TenantSpec

TenantSpec describes an organization (or sub-org) that owns one or more
Platforms. Tenant is cluster-scoped — it doesn't represent a Kubernetes
namespace; it represents an organizational boundary that crosses
Platforms. The relationship to Platform is by `Platform.spec.tenant`
referencing `Tenant.metadata.name`.

_Appears in:_

- [Tenant](#tenant)

| Field                                            | Description                                                                                                                                                                                                                                                                                                                            | Default | Validation                                                                       |
| ------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | -------------------------------------------------------------------------------- |
| `displayName` _string_                           | DisplayName is the human-readable tenant name shown in dashboards<br />and persona UX.                                                                                                                                                                                                                                                 |         | Optional: \{\} <br />                                                            |
| `primaryPersona` _string_                        | PrimaryPersona drives default values for new Platforms onboarded<br />into this tenant. One of the standard persona names.                                                                                                                                                                                                             | generic | Enum: [sales-ops support finance ops founder eng marketing legal generic] <br /> |
| `contact` _[ContactSpec](#contactspec)_          | Contact carries human-readable owner info (Slack channel, on-call<br />rotation, billing email) for ops to reach.                                                                                                                                                                                                                      |         | Optional: \{\} <br />                                                            |
| `compliance` _[ComplianceSpec](#compliancespec)_ | Compliance baseline applied to every Platform owned by this Tenant<br />unless the Platform itself sets a stricter value.                                                                                                                                                                                                              |         | Optional: \{\} <br />                                                            |
| `aggregateMonthlyBudgetUsd` _string_             | AggregateMonthlyBudgetUsd is the soft cap on the SUM of all owned<br />Platforms' BudgetPolicy.spec.monthlyUsd. Status reports whether the<br />sum exceeds this; the operator does not enforce — each Platform's<br />own BudgetPolicy is the enforcement layer. Modeled as a decimal-<br />string to mirror BudgetPolicy.monthlyUsd. |         | Pattern: `^[0-9]+(\.[0-9]\{1,2\})?$` <br />Optional: \{\} <br />                 |

#### TenantStatus

TenantStatus aggregates the state of Platforms owned by this Tenant.

_Appears in:_

- [Tenant](#tenant)

| Field                                                                                                                    | Description                                                                                                                        | Default | Validation            |
| ------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- | ------- | --------------------- |
| `phase` _string_                                                                                                         | Phase: Pending, Active, Suspended (any owned Platform suspended),<br />Failed.                                                     |         | Optional: \{\} <br /> |
| `platformCount` _integer_                                                                                                | PlatformCount is the number of Platform CRs whose<br />spec.tenant == Tenant.metadata.name.                                        |         | Optional: \{\} <br /> |
| `readyPlatformCount` _integer_                                                                                           | ReadyPlatformCount is the subset of PlatformCount in phase=Ready.                                                                  |         | Optional: \{\} <br /> |
| `suspendedPlatformCount` _integer_                                                                                       | SuspendedPlatformCount is the subset in phase=Suspended.                                                                           |         | Optional: \{\} <br /> |
| `aggregateSpendUsd` _string_                                                                                             | AggregateSpendUsd is the sum of CurrentSpendUsd across all owned<br />BudgetPolicies (one per owned Platform).                     |         | Optional: \{\} <br /> |
| `aggregateBudgetUsd` _string_                                                                                            | AggregateBudgetUsd is the sum of MonthlyUsd across all owned<br />BudgetPolicies.                                                  |         | Optional: \{\} <br /> |
| `percentOfBudget` _integer_                                                                                              | PercentOfBudget — 0..200+. Computed from AggregateSpend /<br />AggregateBudget. When > 100 a TenantBudgetExceeded condition fires. |         | Optional: \{\} <br /> |
| `lastReconciled` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#time-v1-meta)_             | LastReconciled timestamp.                                                                                                          |         | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.33/#condition-v1-meta) array_ |                                                                                                                                    |         | Optional: \{\} <br /> |

#### ToolRef

ToolRef references a kagent ToolServer by name.

_Appears in:_

- [AgentSpec](#agentspec)

| Field           | Description | Default | Validation |
| --------------- | ----------- | ------- | ---------- |
| `name` _string_ |             |         |            |
