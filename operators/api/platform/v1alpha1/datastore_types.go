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

	// DeletionPolicy governs the underlying AWS resource when this datastore is
	// removed from spec or the Platform is deleted (T2).
	//   Retain (default): the resource is orphaned, tagged
	//     platform.nanohype.dev/owned-by and platform.nanohype.dev/released-at,
	//     so a `kubectl delete platform` never takes the data with it.
	//   Delete: the resource is torn down with the declaration.
	// Independent of the per-kind deletion_protection backstop, which defaults on
	// for relational and cache — two gates, both defaulting closed.
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
type RelationalConfig struct {
	// EngineVersion of Aurora PostgreSQL. Drift: reported, never converged — an
	// out-of-band engine change is not force-corrected because a downgrade is
	// destructive; the operator raises a Drifted condition instead.
	// +kubebuilder:default="16.6"
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`

	// MinACU is the Serverless v2 floor in Aurora Capacity Units, in 0.5-ACU
	// steps (e.g. "0.5", "1", "8"). Serialized as a string, per the Kubernetes
	// convention for fractional values. The exact 0.5–256 range and the
	// maxACU >= minACU relation are enforced at the tenant-substrate module's
	// variable boundary. Drift: converged — the operator resets scaling bounds
	// to spec.
	// +kubebuilder:validation:Pattern=`^([1-9][0-9]{0,2}(\.5)?|0\.5)$`
	// +kubebuilder:default="0.5"
	// +optional
	MinACU string `json:"minACU,omitempty"`

	// MaxACU is the Serverless v2 ceiling, in 0.5-ACU steps. Drift: converged.
	// +kubebuilder:validation:Pattern=`^([1-9][0-9]{0,2}(\.5)?|0\.5)$`
	// +kubebuilder:default="8"
	// +optional
	MaxACU string `json:"maxACU,omitempty"`

	// BackupRetentionDays for automated backups. Drift: converged.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=35
	// +kubebuilder:default=7
	// +optional
	BackupRetentionDays int32 `json:"backupRetentionDays,omitempty"`

	// DeletionProtection is the AWS-level backstop (T2/(c)): with it on, the
	// cluster cannot be deleted even by an authorized principal until it is
	// cleared. Defaults on. Drift: converged.
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
// (AWS recreates the index to change it); drift on projection is reported.
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

// KeyValueConfig tunes the DynamoDB table. The key schema is immutable; billing
// mode, TTL, and point-in-time recovery converge on drift.
type KeyValueConfig struct {
	// PartitionKey (hash key). Immutable after create.
	PartitionKey AttributeSchema `json:"partitionKey"`

	// SortKey (range key). Immutable after create.
	// +optional
	SortKey *AttributeSchema `json:"sortKey,omitempty"`

	// BillingMode. PAY_PER_REQUEST (default) suits a young tenant with unknown
	// traffic; PROVISIONED is for steady, predictable load. Drift: converged.
	// +kubebuilder:validation:Enum=PAY_PER_REQUEST;PROVISIONED
	// +kubebuilder:default=PAY_PER_REQUEST
	// +optional
	BillingMode string `json:"billingMode,omitempty"`

	// TTLAttribute names the item attribute holding an epoch expiry; empty
	// disables TTL. Drift: converged.
	// +optional
	TTLAttribute string `json:"ttlAttribute,omitempty"`

	// PointInTimeRecovery enables continuous backups. Defaults on. Drift:
	// converged.
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
// are always on and not configurable. Both fields converge on drift.
type ObjectStoreConfig struct {
	// Versioning keeps prior object versions. Defaults on; set false only for a
	// bucket of regenerable data where prior versions add cost with no recovery
	// value. Drift: converged.
	// +kubebuilder:default=true
	// +optional
	Versioning *bool `json:"versioning,omitempty"`

	// LifecycleExpireDays expires objects after N days; 0 (default) keeps them
	// indefinitely. Drift: converged.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	LifecycleExpireDays int32 `json:"lifecycleExpireDays,omitempty"`
}

// QueueConfig tunes the SQS queue. FIFO-ness is immutable (a FIFO and a standard
// queue are different resources); the remaining fields converge on drift.
type QueueConfig struct {
	// FIFO makes an exactly-once, ordered queue. Immutable after create.
	// +kubebuilder:default=false
	// +optional
	FIFO *bool `json:"fifo,omitempty"`

	// VisibilityTimeoutSeconds before a received-but-unacked message is
	// redelivered. Drift: converged.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=43200
	// +kubebuilder:default=30
	// +optional
	VisibilityTimeoutSeconds int32 `json:"visibilityTimeoutSeconds,omitempty"`

	// MessageRetentionSeconds a message is kept before it expires (default 4
	// days). Drift: converged.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=1209600
	// +kubebuilder:default=345600
	// +optional
	MessageRetentionSeconds int32 `json:"messageRetentionSeconds,omitempty"`

	// MaxReceiveCount, when > 0, provisions a dead-letter queue and redrives a
	// message to it after this many failed receives; 0 (default) means no DLQ.
	// Drift: converged.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=0
	// +optional
	MaxReceiveCount int32 `json:"maxReceiveCount,omitempty"`
}

// CacheConfig tunes the ElastiCache cluster. Engine and node type are reported
// on drift (a resize is disruptive); replica count converges.
type CacheConfig struct {
	// Engine of the cache. Valkey is the default going-forward OSS engine.
	// Drift: reported.
	// +kubebuilder:validation:Enum=valkey;redis
	// +kubebuilder:default=valkey
	// +optional
	Engine string `json:"engine,omitempty"`

	// NodeType sizes each node. Drift: reported — a node-type change is a
	// disruptive resize, surfaced as a condition rather than force-applied.
	// +kubebuilder:default="cache.t4g.micro"
	// +optional
	NodeType string `json:"nodeType,omitempty"`

	// Replicas is the number of read replicas per shard; 0 (default) is a
	// single-node cache for a young tenant. Drift: converged.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:default=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
}

// DatastoreStatus reports one datastore's observed state (T3/(a)). It lives
// under PlatformStatus.Datastores, separate from the top-level Phase so a
// still-creating datastore does not hold back the tenant's Ready (T6).
type DatastoreStatus struct {
	// Name matches spec.datastores[].name.
	Name string `json:"name"`

	// Kind echoes the declared kind.
	// +optional
	Kind DatastoreKind `json:"kind,omitempty"`

	// Phase: Pending, Provisioning, Ready, Drifted, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Endpoint is the connection address once available — Aurora/cache endpoint,
	// SQS queue URL, S3 bucket name, or MSK bootstrap brokers.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ARN of the provisioned resource.
	// +optional
	ARN string `json:"arn,omitempty"`

	// SecretName is the resolved name of the credentials secret the datastore
	// publishes — the RDS-managed master secret for relational — so the tenant
	// chart reads one predictable place instead of hand-wiring it per app (T7).
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Drift lists spec fields observed to differ from AWS that the operator
	// reports but does not converge (the destructive-to-correct fields per T3).
	// Empty when in sync.
	// +optional
	Drift []string `json:"drift,omitempty"`
}
