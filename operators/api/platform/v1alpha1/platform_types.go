/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlatformSpec defines the desired state of a Platform — a tenancy boundary
// hosting one or more AgentFleets, with its own budget, identity, and
// guardrails.
type PlatformSpec struct {
	// DisplayName is a human-readable name for dashboards and CLI output.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Persona drives default values for AgentFleet, ModelGateway, and
	// dashboards. One of: sales-ops, support, finance, ops, founder, eng,
	// marketing, legal, generic.
	// +kubebuilder:validation:Enum=sales-ops;support;finance;ops;founder;eng;marketing;legal;generic
	// +kubebuilder:default=generic
	Persona string `json:"persona"`

	// Tenant is the owning Tenant CR (one Tenant can own multiple Platforms).
	Tenant string `json:"tenant"`

	// Budget references a BudgetPolicy CR in the same namespace.
	Budget BudgetRef `json:"budget"`

	// Identity controls how the IRSA role is named + which Bedrock models are
	// reachable.
	Identity IdentitySpec `json:"identity"`

	// Compliance flags drive stricter defaults across the Platform.
	// +optional
	Compliance ComplianceSpec `json:"compliance,omitempty"`

	// Isolation is the workload-isolation tier:
	//   - namespace (default): namespace RBAC + default-deny NetworkPolicy +
	//     ResourceQuota + PSS-restricted, tenant workloads on the host API server.
	//   - vcluster: the same host-side containment PLUS a per-Platform virtual
	//     cluster, so tenant code that talks to the Kubernetes API talks to its own
	//     API server, not the host's (API-server-level isolation — NOT kernel/node
	//     isolation; see docs/adr/0009-vcluster-isolation-tier.md and SECURITY.md).
	//
	// Immutable: switching tiers on a live Platform is a migration (it would strand
	// the virtual cluster and its synced host objects), so the tier is fixed at
	// create time. Re-declare the Platform to change it. Enforced at admission by
	// the CEL transition rule below — an invalid tier flip fails the apply rather
	// than silently half-reconciling.
	// +kubebuilder:validation:Enum=namespace;vcluster
	// +kubebuilder:default=namespace
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="isolation is immutable; re-create the Platform to change its isolation tier"
	// +optional
	Isolation string `json:"isolation,omitempty"`

	// Attribution opts the Platform into per-session human attribution. When
	// set, the operator provisions a session role — assumable by the tenant
	// IRSA role with the operator carried as STS SourceIdentity, scoped to the
	// tenant baseline (Bedrock invoke) and NOT broad sts:AssumeRole — plus a
	// ClusterRole letting the tenant ServiceAccount impersonate the named
	// operators at the apiserver. fab's role-session entrypoint consumes both,
	// so an agent's AWS + Kubernetes actions attribute to a named human.
	// nil = unattributed (the default).
	// +optional
	Attribution *AttributionSpec `json:"attribution,omitempty"`

	// Datastores declares the tenant's stateful substrate — the databases,
	// buckets, queues, caches, and streams it needs. Each entry is a declaration,
	// not a hand-written component: the tenant-substrate tofu module provisions
	// the heavy resource from this same list and the operator generates the
	// scoped IAM policy that reaches it, so adding a tenant never means authoring
	// a landing-zone component. Empty for a Platform with no stateful needs.
	// +optional
	// +listType=map
	// +listMapKey=name
	Datastores []DatastoreSpec `json:"datastores,omitempty"`
}

// AttributionSpec configures per-session human attribution for a Platform. See
// github.com/nanohype/fab docs/attribution.md for the consumer side.
type AttributionSpec struct {
	// Operators is the set of human identities (e.g. email addresses) a
	// session in this Platform may act as. Each value becomes both an allowed
	// STS SourceIdentity on the session role's trust policy and a resourceNames
	// entry on the impersonate ClusterRole, so the SAME string binds the AWS
	// and Kubernetes audit records. Use a canonical form (a lowercased email);
	// it must byte-match the operator's own RBAC subject name.
	// +kubebuilder:validation:MinItems=1
	Operators []string `json:"operators"`

	// SessionRoleMaxDurationSeconds caps the assumed session lifetime. Because
	// the caller is the tenant IRSA role, AWS STS role chaining hard-caps a
	// chained session at 3600s regardless of this value; larger values only
	// matter if the caller ever changes. Defaults to 3600.
	// +kubebuilder:validation:Minimum=900
	// +kubebuilder:validation:Maximum=43200
	// +kubebuilder:default=3600
	// +optional
	SessionRoleMaxDurationSeconds *int32 `json:"sessionRoleMaxDurationSeconds,omitempty"`
}

// BudgetRef points at a BudgetPolicy by name.
type BudgetRef struct {
	Name string `json:"name"`
}

// Capability is a managed AWS capability the datastore vocabulary does not
// cover. Declaring one drives an operator-generated `capability-access` inline
// policy on the tenant role, scoped by the same <env>-<platform> naming
// convention the datastore policy uses — so a capability is a statement of
// need, not a hand-written managed policy the tenant references by ARN.
//
//	ses                  -> ses:SendEmail scoped by a ses:FromAddress condition
//	                        to the tenant's sending domain. The verified sending
//	                        identity itself is account-level mail infra
//	                        (landing-zone), not provisioned here.
//	eventBridgeScheduler -> scheduler:*Schedule on the tenant's own schedules
//	                        plus an operator-minted <env>-<platform>-scheduler-invoke
//	                        role (trusted by scheduler.amazonaws.com, allowed to
//	                        SendMessage to the tenant's own queue datastores) that
//	                        the tenant passes when creating a schedule.
//
// +kubebuilder:validation:Enum=ses;eventBridgeScheduler
type Capability string

// The capability vocabulary. Each maps to the operator-generated grants
// documented on Capability.
const (
	CapabilitySES                  Capability = "ses"
	CapabilityEventBridgeScheduler Capability = "eventBridgeScheduler"
)

// IdentitySpec wires the per-Platform IRSA role. The controller reconciles a
// `bedrock-model-scoping` inline policy onto the tenant role (and the
// attribution session role, when spec.attribution is set) that denies the
// Bedrock model-invoke actions (InvokeModel, InvokeModelWithResponseStream,
// Converse, ConverseStream) on every resource outside the set that
// AllowedModels / AllowedModelFamilies expand to. The baseline policy's broad
// invoke grant is thereby narrowed to exactly the declared models; when
// neither field is set the policy denies all model invocation
// (deny-by-default).
// +kubebuilder:validation:XValidation:rule="!(has(self.allowedModels) && size(self.allowedModels) > 0 && has(self.allowedModelFamilies) && size(self.allowedModelFamilies) > 0)",message="allowedModels and allowedModelFamilies are mutually exclusive"
type IdentitySpec struct {
	// AllowedModels is the list of Bedrock model IDs or cross-region
	// inference-profile IDs (e.g. "anthropic.claude-sonnet-4-6",
	// "us.anthropic.claude-sonnet-4-6-v1:0") the role may invoke. The
	// controller expands each entry into its foundation-model ARN pattern plus
	// the matching inference-profile ARN pattern (a `us.` profile fans out to
	// foundation models across regions, so both are granted together) and
	// reconciles them into the role's bedrock-model-scoping policy. Scopes
	// tighter than a family; mutually exclusive with AllowedModelFamilies.
	// +optional
	AllowedModels []string `json:"allowedModels,omitempty"`

	// AllowedModelFamilies (e.g. ["anthropic", "amazon-nova"]) is expanded by
	// the controller at reconcile time into the family's foundation-model ARN
	// pattern (arn:<partition>:bedrock:*::foundation-model/<prefix>*) and, for
	// families with cross-region inference profiles (anthropic, amazon-nova,
	// meta, mistral), the `us.` inference-profile ARN pattern
	// (arn:<partition>:bedrock:<region>:<account>:inference-profile/us.<prefix>*),
	// then reconciled into the role's bedrock-model-scoping policy. Leaving
	// both this and AllowedModels empty denies all Bedrock model invocation
	// for the Platform's roles.
	// +kubebuilder:validation:items:Enum=anthropic;amazon-nova;amazon-titan;meta;mistral;cohere;stability
	// +optional
	AllowedModelFamilies []string `json:"allowedModelFamilies,omitempty"`

	// ExtraPolicyArns are managed IAM policies attached on top of the baseline.
	// +optional
	ExtraPolicyArns []string `json:"extraPolicyArns,omitempty"`

	// Capabilities are managed AWS capabilities outside the datastore vocabulary
	// (SES send, EventBridge Scheduler). Each drives an operator-generated
	// `capability-access` inline policy — and, for eventBridgeScheduler, a minted
	// scheduler-invoke role — so a tenant declares what it needs rather than
	// referencing a hand-written managed policy through extraPolicyArns.
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Capabilities []Capability `json:"capabilities,omitempty"`
}

// ComplianceSpec enables stricter defaults.
type ComplianceSpec struct {
	// HIPAA: object-lock compliance mode, no cross-region inference, PII detect
	// required on Guardrails.
	// +optional
	HIPAA bool `json:"hipaa,omitempty"`

	// SOC2: invocation logging required, kill-switch enabled.
	// +optional
	SOC2 bool `json:"soc2,omitempty"`
}

// PlatformStatus captures the controller's view of the world.
type PlatformStatus struct {
	// Phase: Pending, Provisioning, Ready, Suspended, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// IamRoleArn is the per-Platform IRSA role created by the controller.
	// +optional
	IamRoleArn string `json:"iamRoleArn,omitempty"`

	// SessionRoleArn is the per-Platform attribution session role, created when
	// spec.attribution is set. Empty when attribution is off.
	// +optional
	SessionRoleArn string `json:"sessionRoleArn,omitempty"`

	// Namespace is the tenant namespace the controller provisioned.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ObservedGeneration is the last spec.generation the controller reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SuspendedAt is the timestamp at which the kill-switch fired. When
	// non-nil the operator stops reattaching the baseline IAM policy and
	// the AgentFleetReconciler scales fleets to zero. Resets to nil only
	// when ops clears the iam:TagRole 'platform.nanohype.dev/suspended'
	// marker on the tenant IRSA role.
	// +optional
	SuspendedAt *metav1.Time `json:"suspendedAt,omitempty"`

	// SuspendedReason carries the kill-switch's reason (e.g.
	// 'budget-exceeded'). Same lifecycle as SuspendedAt.
	// +optional
	SuspendedReason string `json:"suspendedReason,omitempty"`

	// Conditions follows the standard kubernetes pattern.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Datastores reports per-datastore observed state, separate from the
	// top-level Phase: a Platform is Ready once its namespace, quota, and
	// identity are live, while each datastore reports its own readiness here so a
	// still-creating Aurora cluster does not gate the tenant's Ready (T6).
	// +optional
	// +listType=map
	// +listMapKey=name
	Datastores []DatastoreStatus `json:"datastores,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=plat
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Persona",type=string,JSONPath=`.spec.persona`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!has(self.spec.datastores) || self.spec.datastores.all(d, size(self.metadata.name) + size(d.name) <= 28)",message="platform name + datastore name must be <= 28 combined: they compose into S3 bucket and table/queue names as <env>-<name>-<datastore>-<account>-<suffix> against S3's 63-char ceiling (63 - env<=11 - account 12 - 4 separators - suffix<=8); the tenant-substrate module re-proves the exact env/account-aware length"

// Platform is the top-level tenancy CR. Namespaced so that BudgetPolicy,
// ModelGateway, AgentFleet, and EvalSuite references resolve in the same
// namespace by name. The operator provisions the tenant workload namespace
// (tenants-<platform-name>) separately at reconcile time; the Platform CR
// itself lives in whichever namespace the cluster admin places it (typically
// a management namespace such as eks-agent-platform).
type Platform struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformSpec   `json:"spec,omitempty"`
	Status PlatformStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlatformList is the list-form of Platform.
type PlatformList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Platform `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Platform{}, &PlatformList{})
}
