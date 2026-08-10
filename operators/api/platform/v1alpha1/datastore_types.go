/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package v1alpha1

// DatastoreKind is the abstract kind of a tenant datastore. The Platform CR
// names what the tenant needs; the operator and the tenant-substrate tofu
// module map each kind to an AWS implementation and scope access to it. Keeping
// the vocabulary abstract preserves the pluggable seam the org commits to
// elsewhere and keeps the spec a statement of need rather than a config file
// for a specific service.
//
//	relational  -> Aurora PostgreSQL Serverless v2
//	keyValue    -> DynamoDB
//	objectStore -> S3
//	queue       -> SQS (with a dead-letter queue when redrive is set)
//	cache       -> ElastiCache (Valkey / Redis)
//	stream      -> MSK Serverless (IAM auth)
//
// +kubebuilder:validation:Enum=relational;keyValue;objectStore;queue;cache;stream
type DatastoreKind string

// The datastore kinds. Each maps to the AWS implementation documented on
// DatastoreKind.
const (
	DatastoreRelational  DatastoreKind = "relational"
	DatastoreKeyValue    DatastoreKind = "keyValue"
	DatastoreObjectStore DatastoreKind = "objectStore"
	DatastoreQueue       DatastoreKind = "queue"
	DatastoreCache       DatastoreKind = "cache"
	DatastoreStream      DatastoreKind = "stream"
)

// DatastoreSpec declares one stateful store the tenant needs. The kind selects
// an AWS implementation and, at most, the one typed config block matching that
// kind (stream needs none; a kind whose block is omitted takes the young/light
// defaults). The heavy resource is provisioned by the tenant-substrate tofu
// module; the operator generates the scoped IAM policy that reaches it. Nothing
// here grants the operator delete on the store — deletion is governed by
// deletionPolicy and the per-kind deletion_protection backstop, not by the
// reconciling principal's IAM (T1/T2).
//
// Drift, stated once here rather than per field, because it is the same answer
// for every field: the operator does not observe it. It holds no AWS client for
// RDS, DynamoDB, ElastiCache, SQS or MSK, so it cannot read a live datastore's
// configuration, let alone converge one. Drift between this declaration and the
// provisioned resource is detected where the resource is owned — landing-zone's
// scheduled `tofu plan` (`drift.yml`), which opens a GitHub issue. It does not
// reach the CR, and no datastore status field reports it.
//
// +kubebuilder:validation:XValidation:rule="(!has(self.relational) || self.kind == 'relational') && (!has(self.keyValue) || self.kind == 'keyValue') && (!has(self.objectStore) || self.kind == 'objectStore') && (!has(self.queue) || self.kind == 'queue') && (!has(self.cache) || self.kind == 'cache')",message="a datastore's config block must match its kind (e.g. kind=relational may only set the 'relational' block); kind=stream carries no block"
// +kubebuilder:validation:XValidation:rule="self.kind != 'keyValue' || has(self.keyValue)",message="kind=keyValue requires the 'keyValue' block: a DynamoDB table has no default partition key. Every other kind may omit its block to take the young/light defaults"
type DatastoreSpec struct {
	// Name identifies the datastore within its Platform and composes into the
	// AWS resource names (bucket, table, queue, cluster) alongside the env,
	// account, and platform tokens. A short RFC-1123 label; the tenant-substrate
	// module re-proves the exact composed length at its variable boundary, where
	// the env and account values are known.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,16}[a-z0-9])?$`
	Name string `json:"name"`

	// Kind selects the AWS implementation. Immutable — changing a live
	// datastore's kind would strand the provisioned resource.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="datastore kind is immutable; remove the datastore and declare a new one to change it"
	Kind DatastoreKind `json:"kind"`

	// DeletionPolicy declares whether this datastore's AWS resource should
	// survive a teardown of the substrate that provisioned it.
	//
	// It does NOT govern `kubectl delete platform`. The operator holds no delete
	// permission on any datastore, so deleting the CR orphans every store
	// regardless of this field — the isolation boundary is enforced by
	// permission, not by policy. This field reaches only the tenant-substrate
	// module's destroy path.
	//
	// What it means is per-kind, because the same word cannot name the same
	// mechanism across services that offer different ones:
	//   keyValue: Retain arms DynamoDB deletion protection, which refuses the
	//     delete outright and leaves no cost behind. The clean case.
	//   relational: Retain takes a final Aurora snapshot; Delete skips it. The
	//     snapshot outlives the cluster and begins billing per GB-month once it
	//     falls outside backupRetentionDays, so Retain here is durability with a
	//     cost tail, not free protection. Aurora's own deletionProtection is a
	//     separate and independent gate: Delete alone will not drop a protected
	//     cluster, both must open.
	//   objectStore, queue, cache, stream: no effect. S3 has no deletion
	//     protection (force_destroy governs emptying a bucket, not deleting an
	//     empty one), and SQS, ElastiCache and MSK offer no retain lever at all.
	//     Cache is derived data by design and is deliberately unprotected.
	//
	// The operator's substrate-wide teardown lever overrides this in every case.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// Relational config; honored only when kind=relational.
	// +optional
	Relational *RelationalConfig `json:"relational,omitempty"`

	// KeyValue config; honored only when kind=keyValue.
	// +optional
	KeyValue *KeyValueConfig `json:"keyValue,omitempty"`

	// ObjectStore config; honored only when kind=objectStore.
	// +optional
	ObjectStore *ObjectStoreConfig `json:"objectStore,omitempty"`

	// Queue config; honored only when kind=queue.
	// +optional
	Queue *QueueConfig `json:"queue,omitempty"`

	// Cache config; honored only when kind=cache.
	// +optional
	Cache *CacheConfig `json:"cache,omitempty"`
}

// RelationalConfig tunes the Aurora PostgreSQL Serverless v2 cluster. Omitting
// the block provisions the young/light default: 0.5–8 ACU, 7-day backups,
// deletion protection on.
//
// +kubebuilder:validation:XValidation:rule="double(self.maxACU) >= double(self.minACU)",message="maxACU must be >= minACU"
// +kubebuilder:validation:XValidation:rule="self.minACU != '0' || int(self.engineVersion.split('.')[0]) >= 16",message="minACU '0' is Aurora Serverless v2 auto-pause, which requires Aurora PostgreSQL 16.3 or later; raise engineVersion or set a non-zero floor"
type RelationalConfig struct {
	// EngineVersion of Aurora PostgreSQL, as a major line ("16") or a full
	// version ("16.14").
	//
	// The major line is the default and the one to reach for. RDS resolves it to
	// the newest minor the region offers, so there is no patch number left to go
	// stale — and a stale one is not a theoretical cost. Pinned at "16.6", every
	// apply of a relational datastore failed with `Cannot find version 16.6 for
	// aurora-postgresql` once AWS withdrew it, after the VPC and the EKS cluster
	// were already built and billing. A withdrawn version is a fact about the
	// region rather than a change to any manifest, so nothing in CI can see it
	// coming.
	//
	// Pin a full version only to hold a tenant on one deliberately, and expect
	// to move it when AWS retires that version too.
	//
	// Auto-pause (minACU "0") needs 16.3 or later, which the rule on this type
	// enforces against the major. A major-only value satisfies it: every 16.x
	// AWS still offers is past the floor, so "whatever is current" cannot
	// resolve below it. A full version below the floor on the same major —
	// 16.0 to 16.2 — is still admitted and still fails at apply, because the
	// minor is not machine-checkable here.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +kubebuilder:validation:MaxLength=10
	// +kubebuilder:default="16"
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`

	// MinACU is the Serverless v2 floor in Aurora Capacity Units, in 0.5-ACU
	// steps (e.g. "0.5", "1", "8"). Serialized as a string, per the Kubernetes
	// convention for fractional values.
	//
	// "0" is the auto-pause floor: the cluster scales to zero compute after five
	// idle minutes and a later connection resumes it, at the cost of roughly
	// fifteen seconds on that first connect. It pauses only while nothing is
	// connected, so a workload holding a pool open — or probing readiness with a
	// query — never reaches it and bills the same as "0.5". Storage, IO, backup
	// and KMS bill through a pause regardless; only compute stops.
	// +kubebuilder:validation:Pattern=`^(0|0\.5|([1-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])(\.5)?|256)$`
	// +kubebuilder:validation:MaxLength=5
	// +kubebuilder:default="0.5"
	// +optional
	MinACU string `json:"minACU,omitempty"`

	// MaxACU is the Serverless v2 ceiling, in 0.5-ACU steps. Unlike the floor it
	// cannot be "0" — a ceiling of zero leaves no capacity to scale into.
	// +kubebuilder:validation:Pattern=`^(0\.5|([1-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])(\.5)?|256)$`
	// +kubebuilder:validation:MaxLength=5
	// +kubebuilder:default="8"
	// +optional
	MaxACU string `json:"maxACU,omitempty"`

	// BackupRetentionDays for automated backups.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=35
	// +kubebuilder:default=7
	// +optional
	BackupRetentionDays int32 `json:"backupRetentionDays,omitempty"`

	// DeletionProtection is the AWS-level backstop (T2/(c)): with it on, the
	// cluster cannot be deleted even by an authorized principal until it is
	// cleared. Defaults on.
	// +kubebuilder:default=true
	// +optional
	DeletionProtection *bool `json:"deletionProtection,omitempty"`
}

// AttributeSchema names a DynamoDB key attribute and its scalar type
// (S string, N number, B binary).
type AttributeSchema struct {
	// Name of the attribute.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{1,255}$`
	Name string `json:"name"`

	// Type is the DynamoDB scalar attribute type.
	// +kubebuilder:validation:Enum=S;N;B
	Type string `json:"type"`
}

// GlobalSecondaryIndex declares a DynamoDB GSI. The key schema is immutable
// (AWS recreates the index to change it).
type GlobalSecondaryIndex struct {
	// Name of the index.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]{3,255}$`
	Name string `json:"name"`

	// PartitionKey (hash key) of the index.
	PartitionKey AttributeSchema `json:"partitionKey"`

	// SortKey (range key) of the index.
	// +optional
	SortKey *AttributeSchema `json:"sortKey,omitempty"`

	// Projection controls which attributes are copied into the index.
	// +kubebuilder:validation:Enum=ALL;KEYS_ONLY;INCLUDE
	// +kubebuilder:default=ALL
	// +optional
	Projection string `json:"projection,omitempty"`
}

// KeyValueConfig tunes the DynamoDB table. The key schema is immutable.
type KeyValueConfig struct {
	// PartitionKey (hash key). Immutable after create.
	PartitionKey AttributeSchema `json:"partitionKey"`

	// SortKey (range key). Immutable after create.
	// +optional
	SortKey *AttributeSchema `json:"sortKey,omitempty"`

	// BillingMode. PAY_PER_REQUEST (default) suits a young tenant with unknown
	// traffic; PROVISIONED is for steady, predictable load.
	// +kubebuilder:validation:Enum=PAY_PER_REQUEST;PROVISIONED
	// +kubebuilder:default=PAY_PER_REQUEST
	// +optional
	BillingMode string `json:"billingMode,omitempty"`

	// TTLAttribute names the item attribute holding an epoch expiry; empty
	// disables TTL.
	// +optional
	TTLAttribute string `json:"ttlAttribute,omitempty"`

	// PointInTimeRecovery enables continuous backups. Defaults on.
	// +kubebuilder:default=true
	// +optional
	PointInTimeRecovery *bool `json:"pointInTimeRecovery,omitempty"`

	// GlobalSecondaryIndexes declared on the table.
	// +optional
	// +listType=map
	// +listMapKey=name
	GlobalSecondaryIndexes []GlobalSecondaryIndex `json:"globalSecondaryIndexes,omitempty"`
}

// ObjectStoreConfig tunes the S3 bucket. Encryption and public-access blocking
// are always on and not configurable.
type ObjectStoreConfig struct {
	// Versioning keeps prior object versions. Defaults on; set false only for a
	// bucket of regenerable data where prior versions add cost with no recovery
	// value.
	// +kubebuilder:default=true
	// +optional
	Versioning *bool `json:"versioning,omitempty"`

	// LifecycleExpireDays expires objects after N days; 0 (default) keeps them
	// indefinitely.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	LifecycleExpireDays int32 `json:"lifecycleExpireDays,omitempty"`
}

// QueueConfig tunes the SQS queue. FIFO-ness is immutable (a FIFO and a standard
// queue are different resources).
type QueueConfig struct {
	// FIFO makes an exactly-once, ordered queue. Immutable after create.
	// +kubebuilder:default=false
	// +optional
	FIFO *bool `json:"fifo,omitempty"`

	// VisibilityTimeoutSeconds before a received-but-unacked message is
	// redelivered.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=43200
	// +kubebuilder:default=30
	// +optional
	VisibilityTimeoutSeconds int32 `json:"visibilityTimeoutSeconds,omitempty"`

	// MessageRetentionSeconds a message is kept before it expires (default 4
	// days).
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=1209600
	// +kubebuilder:default=345600
	// +optional
	MessageRetentionSeconds int32 `json:"messageRetentionSeconds,omitempty"`

	// MaxReceiveCount, when > 0, provisions a dead-letter queue and redrives a
	// message to it after this many failed receives; 0 (default) means no DLQ.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=0
	// +optional
	MaxReceiveCount int32 `json:"maxReceiveCount,omitempty"`
}

// CacheConfig tunes the ElastiCache cluster. Changing engine or node type on a
// live cluster is a disruptive replacement, so treat both as set-at-create.
type CacheConfig struct {
	// Engine of the cache. Valkey is the default going-forward OSS engine.
	// +kubebuilder:validation:Enum=valkey;redis
	// +kubebuilder:default=valkey
	// +optional
	Engine string `json:"engine,omitempty"`

	// NodeType sizes each node. Changing it on a live cluster is a disruptive
	// resize.
	// +kubebuilder:default="cache.t4g.micro"
	// +optional
	NodeType string `json:"nodeType,omitempty"`

	// Replicas is the number of read replicas per shard; 0 (default) is a
	// single-node cache for a young tenant.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:default=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
}

// DatastoreStatus reports the stable identity a tenant uses to reach one
// declared datastore. It lives under PlatformStatus.Datastores.
//
// It is an identity report, not a liveness report. Every value here is composed
// from the <env>-<platform>-<datastore> convention that the tenant-substrate
// module names by and the datastore-access policy scopes to, so it needs no AWS
// call — and makes no claim that the resource exists yet.
type DatastoreStatus struct {
	// Name matches spec.datastores[].name.
	Name string `json:"name"`

	// Kind echoes the declared kind.
	// +optional
	Kind DatastoreKind `json:"kind,omitempty"`

	// Phase is the owning Platform's phase, copied. It reports identity and
	// access readiness, not the datastore's own state — the operator observes
	// no live datastore, so it has nothing else to derive a phase from, and
	// this can never hold a value the Platform cannot.
	// Phase: Pending, Provisioning, Ready, Suspended, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoint is the connection address, and only for the kinds whose name is
	// fully deterministic: the S3 bucket, the DynamoDB table, the SQS queue URL.
	// Aurora, ElastiCache and MSK endpoints carry an AWS-generated id, so this
	// is empty for those three and the address comes from the module's outputs.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ARN of the datastore, composed from the naming convention.
	// +optional
	ARN string `json:"arn,omitempty"`

	// SecretName is the resolved name of the credentials secret the datastore
	// publishes — the RDS-managed master secret for relational — so the tenant
	// chart reads one predictable place instead of hand-wiring it per app (T7).
	// +optional
	SecretName string `json:"secretName,omitempty"`
}
