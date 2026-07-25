/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The optional half of a Platform declaration: the stateful substrate it needs,
// the managed AWS capabilities outside that vocabulary, the application secrets
// its pods read directly, and whether its sessions attribute to a named human.
//
// Both scaffolders share this parser so a tenant scaffolded by `tenant init` and
// one scaffolded by `platform new` are held to the same rules — and to the same
// rules charts/tenant enforces at render time. A scaffold is written to disk or
// piped to kubectl with no admission feedback in between, so every constraint the
// API server or the tenant-substrate tofu module would enforce is checked here
// instead: a scaffold this package emits is one `kubectl apply` accepts.
//
// The flag surface is deliberately the minimum that produces a VALID
// declaration — name, kind, deletion policy, and the DynamoDB key schema that
// has no default. Everything else takes the CRD's young/light defaults, which is
// what a scaffolder should hand you: a correct starting point to edit, not a
// second copy of the CRD expressed as flags.

// PlatformVocabulary is the parsed, validated declaration.
type PlatformVocabulary struct {
	Datastores           []platformv1alpha1.DatastoreSpec
	Capabilities         []platformv1alpha1.Capability
	DirectSecretReads    []string
	AttributionOperators []string
	SessionRoleMaxSecs   int32
}

// Empty reports whether nothing optional was declared, so a caller can leave the
// scaffold byte-identical to the no-vocabulary case.
func (v PlatformVocabulary) Empty() bool {
	return len(v.Datastores) == 0 && len(v.Capabilities) == 0 &&
		len(v.DirectSecretReads) == 0 && len(v.AttributionOperators) == 0
}

// Attribution renders the AttributionSpec, or nil when no operators were named.
// Attribution is opt-in by content: the CRD requires at least one operator
// whenever the block is present, so an empty list must omit the block entirely.
func (v PlatformVocabulary) Attribution() *platformv1alpha1.AttributionSpec {
	if len(v.AttributionOperators) == 0 {
		return nil
	}
	secs := v.SessionRoleMaxSecs
	if secs == 0 {
		secs = defaultSessionRoleMaxSecs
	}
	return &platformv1alpha1.AttributionSpec{
		Operators:                     v.AttributionOperators,
		SessionRoleMaxDurationSeconds: &secs,
	}
}

// VocabularyFlags is the raw repeated-flag input, before parsing.
type VocabularyFlags struct {
	Datastores         []string
	Capabilities       []string
	DirectSecretReads  []string
	Operators          []string
	SessionRoleMaxSecs int32
}

const (
	defaultSessionRoleMaxSecs int32 = 3600
	minSessionRoleMaxSecs     int32 = 900
	maxSessionRoleMaxSecs     int32 = 43200

	maxCapabilities      = 8
	maxDirectSecretReads = 16

	// A datastore name composes with the platform name into the provisioned
	// resource name. The CRD's CEL rule caps the pair at 28 characters (S3's
	// 63-char ceiling less the environment, account and suffix tokens); a cache is
	// one tighter, because ElastiCache caps a replication-group id at 40 including
	// the longest environment token.
	nameBudget      = 28
	cacheNameBudget = 27
)

// DatastoreFlagUsage is the --datastore help text, shared by both commands.
const DatastoreFlagUsage = "datastore to declare, as comma-separated key=value " +
	"(repeatable): name=<label>,kind=relational|keyValue|objectStore|queue|cache|stream" +
	"[,deletionPolicy=Retain|Delete][,partitionKey=<attr>:S|N|B][,sortKey=<attr>:S|N|B]. " +
	"Everything else takes the CRD's defaults — edit the emitted YAML to tune it."

// Mirrors of the CRD's own patterns, so the scaffolder rejects what admission
// would. Kept next to the constants they belong with rather than derived from the
// Go types, which carry the patterns as kubebuilder comments.
var (
	datastoreNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,16}[a-z0-9])?$`)
	secretNameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_+=.@-]*$`)
	attributeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,255}$`)
)

var datastoreKinds = []platformv1alpha1.DatastoreKind{
	platformv1alpha1.DatastoreRelational,
	platformv1alpha1.DatastoreKeyValue,
	platformv1alpha1.DatastoreObjectStore,
	platformv1alpha1.DatastoreQueue,
	platformv1alpha1.DatastoreCache,
	platformv1alpha1.DatastoreStream,
}

var capabilityNames = []platformv1alpha1.Capability{
	platformv1alpha1.CapabilitySES,
	platformv1alpha1.CapabilityEventBridgeScheduler,
}

// RegisterVocabularyFlags hangs the vocabulary flags off a command. Both
// scaffolders call it, so the two CLIs never drift on flag name, repeatability,
// or help text.
func RegisterVocabularyFlags(cmd *cobra.Command, flags *VocabularyFlags) {
	cmd.Flags().StringArrayVar(&flags.Datastores, "datastore", nil, DatastoreFlagUsage)
	cmd.Flags().StringArrayVar(&flags.Capabilities, "capability", nil,
		"managed AWS capability to grant (repeatable): "+joinCapabilities()+
			". eventBridgeScheduler needs at least one kind=queue datastore to send to")
	cmd.Flags().StringArrayVar(&flags.DirectSecretReads, "secret-read", nil,
		"application secret the tenant's pods read through the pod role (repeatable). "+
			"Prefix-relative to <platform>/<environment>/, so write oncall/webhook-hmac")
	cmd.Flags().StringArrayVar(&flags.Operators, "attribution-operator", nil,
		"human identity a session in this Platform may act as (repeatable). "+
			"Must byte-match the operator's Kubernetes RBAC subject name; naming one turns attribution on")
	cmd.Flags().Int32Var(&flags.SessionRoleMaxSecs, "session-role-max-seconds", 0,
		"cap on an attributed session's lifetime, 900-43200 (default 3600). "+
			"STS role chaining hard-caps a chained session at 3600 regardless")
}

// ParseVocabulary validates the raw flags against the platform name they will be
// declared on and returns the typed declaration. platformName is required
// because two of the rules are relational: the composed-name budget, and the
// prefix-relative form of a direct secret read.
func ParseVocabulary(platformName string, flags VocabularyFlags) (PlatformVocabulary, error) {
	var out PlatformVocabulary

	seen := map[string]bool{}
	hasQueue := false
	for _, spec := range flags.Datastores {
		d, err := parseDatastore(platformName, spec)
		if err != nil {
			return out, err
		}
		if seen[d.Name] {
			return out, fmt.Errorf("datastore %q declared twice; names are unique within a Platform", d.Name)
		}
		seen[d.Name] = true
		if d.Kind == platformv1alpha1.DatastoreQueue {
			hasQueue = true
		}
		out.Datastores = append(out.Datastores, d)
	}

	if len(flags.Capabilities) > maxCapabilities {
		return out, fmt.Errorf("at most %d capabilities may be declared, got %d", maxCapabilities, len(flags.Capabilities))
	}
	for _, raw := range flags.Capabilities {
		capName := platformv1alpha1.Capability(raw)
		if !hasCapability(capName) {
			return out, fmt.Errorf("unknown capability %q; supported: %s", raw, joinCapabilities())
		}
		// The operator scopes the minted scheduler-invoke role's SendMessage to
		// the tenant's own queue datastores. With none declared the role is
		// created carrying no grant, so the capability is a silent no-op.
		if capName == platformv1alpha1.CapabilityEventBridgeScheduler && !hasQueue {
			return out, fmt.Errorf("capability eventBridgeScheduler needs at least one kind=queue datastore to send to; " +
				"without one the minted scheduler-invoke role is created with no send grant and the capability does nothing")
		}
		out.Capabilities = append(out.Capabilities, capName)
	}

	if len(flags.DirectSecretReads) > maxDirectSecretReads {
		return out, fmt.Errorf("at most %d direct secret reads may be declared, got %d", maxDirectSecretReads, len(flags.DirectSecretReads))
	}
	for _, name := range flags.DirectSecretReads {
		if !secretNameRe.MatchString(name) {
			return out, fmt.Errorf("secret read %q is not a Secrets Manager name: start alphanumeric, then alphanumerics and / _ + = . @ -", name)
		}
		// The operator composes the ARN as <platform>/<environment>/<entry>, so a
		// full path grants on the prefix twice — a valid-looking policy on a
		// secret that does not exist.
		if strings.HasPrefix(name, platformName+"/") {
			return out, fmt.Errorf("secret read %q already starts with the platform name, but entries are prefix-relative: "+
				"the operator composes <platform>/<environment>/<entry>. Drop the leading %q segment", name, platformName)
		}
		out.DirectSecretReads = append(out.DirectSecretReads, name)
	}

	for _, op := range flags.Operators {
		if strings.TrimSpace(op) == "" {
			return out, fmt.Errorf("attribution operator must not be empty")
		}
		// The same string becomes an allowed STS SourceIdentity and a
		// resourceNames entry on the impersonate ClusterRole, so it has to
		// byte-match the operator's own RBAC subject name.
		if op != strings.ToLower(op) {
			return out, fmt.Errorf("attribution operator %q must be lowercase: the same string binds the AWS and Kubernetes audit records, so it has to byte-match the operator's RBAC subject name", op)
		}
		out.AttributionOperators = append(out.AttributionOperators, op)
	}

	secs := flags.SessionRoleMaxSecs
	if secs == 0 {
		secs = defaultSessionRoleMaxSecs
	}
	if secs < minSessionRoleMaxSecs || secs > maxSessionRoleMaxSecs {
		return out, fmt.Errorf("session-role-max-seconds must be between %d and %d, got %d", minSessionRoleMaxSecs, maxSessionRoleMaxSecs, secs)
	}
	if len(out.AttributionOperators) == 0 && flags.SessionRoleMaxSecs != 0 {
		return out, fmt.Errorf("--session-role-max-seconds has no effect without --attribution-operator")
	}
	out.SessionRoleMaxSecs = secs

	return out, nil
}

func parseDatastore(platformName, spec string) (platformv1alpha1.DatastoreSpec, error) {
	var d platformv1alpha1.DatastoreSpec
	fields := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return d, fmt.Errorf("datastore %q: %q is not key=value", spec, pair)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if _, dup := fields[key]; dup {
			return d, fmt.Errorf("datastore %q: key %q given twice", spec, key)
		}
		fields[key] = value
	}

	known := []string{"name", "kind", "deletionPolicy", "partitionKey", "sortKey"}
	for key := range fields {
		if !contains(known, key) {
			sort.Strings(known)
			return d, fmt.Errorf("datastore %q: unknown key %q; supported: %s. Everything else takes the CRD's defaults — edit the emitted YAML", spec, key, strings.Join(known, ", "))
		}
	}

	d.Name = fields["name"]
	if d.Name == "" {
		return d, fmt.Errorf("datastore %q: name= is required", spec)
	}
	if !datastoreNameRe.MatchString(d.Name) {
		return d, fmt.Errorf("datastore %q: name must be a short RFC-1123 label, at most 18 characters (lowercase alphanumeric and hyphens, starting and ending alphanumeric)", d.Name)
	}

	if fields["kind"] == "" {
		return d, fmt.Errorf("datastore %q: kind= is required (%s)", d.Name, joinKinds())
	}
	d.Kind = platformv1alpha1.DatastoreKind(fields["kind"])
	if !hasKind(d.Kind) {
		return d, fmt.Errorf("datastore %q: unknown kind %q (%s)", d.Name, fields["kind"], joinKinds())
	}

	budget := nameBudget
	if d.Kind == platformv1alpha1.DatastoreCache {
		budget = cacheNameBudget
	}
	if combined := len(platformName) + len(d.Name); combined > budget {
		return d, fmt.Errorf("datastore %q: platform name (%s) plus the datastore name is %d characters, over the %d-character budget for kind=%s — they compose into the provisioned resource name",
			d.Name, platformName, combined, budget, d.Kind)
	}

	if policy, ok := fields["deletionPolicy"]; ok {
		if policy != "Retain" && policy != "Delete" {
			return d, fmt.Errorf("datastore %q: deletionPolicy must be Retain or Delete, got %q", d.Name, policy)
		}
		d.DeletionPolicy = policy
	}

	_, hasPK := fields["partitionKey"]
	_, hasSK := fields["sortKey"]
	if d.Kind != platformv1alpha1.DatastoreKeyValue {
		if hasPK || hasSK {
			return d, fmt.Errorf("datastore %q: partitionKey/sortKey only apply to kind=keyValue, not kind=%s", d.Name, d.Kind)
		}
		return d, nil
	}

	// A DynamoDB table has no default partition key, so the CRD requires the
	// keyValue block for this kind and the scaffolder cannot leave it out.
	if !hasPK {
		return d, fmt.Errorf("datastore %q: kind=keyValue requires partitionKey=<attr>:S|N|B — a DynamoDB table has no default partition key", d.Name)
	}
	pk, err := parseAttribute(d.Name, "partitionKey", fields["partitionKey"])
	if err != nil {
		return d, err
	}
	d.KeyValue = &platformv1alpha1.KeyValueConfig{PartitionKey: pk}
	if hasSK {
		sk, err := parseAttribute(d.Name, "sortKey", fields["sortKey"])
		if err != nil {
			return d, err
		}
		d.KeyValue.SortKey = &sk
	}
	return d, nil
}

func parseAttribute(dsName, key, value string) (platformv1alpha1.AttributeSchema, error) {
	var a platformv1alpha1.AttributeSchema
	name, typ, ok := strings.Cut(value, ":")
	if !ok {
		return a, fmt.Errorf("datastore %q: %s must be <attr>:<type>, e.g. %s=sessionId:S", dsName, key, key)
	}
	if !attributeNameRe.MatchString(name) {
		return a, fmt.Errorf("datastore %q: %s attribute name %q is not a DynamoDB attribute name", dsName, key, name)
	}
	if typ != "S" && typ != "N" && typ != "B" {
		return a, fmt.Errorf("datastore %q: %s type must be S, N, or B, got %q", dsName, key, typ)
	}
	return platformv1alpha1.AttributeSchema{Name: name, Type: typ}, nil
}

func hasKind(k platformv1alpha1.DatastoreKind) bool {
	for _, known := range datastoreKinds {
		if k == known {
			return true
		}
	}
	return false
}

func hasCapability(c platformv1alpha1.Capability) bool {
	for _, known := range capabilityNames {
		if c == known {
			return true
		}
	}
	return false
}

func joinKinds() string {
	out := make([]string, len(datastoreKinds))
	for i, k := range datastoreKinds {
		out[i] = string(k)
	}
	return strings.Join(out, "|")
}

func joinCapabilities() string {
	out := make([]string, len(capabilityNames))
	for i, c := range capabilityNames {
		out[i] = string(c)
	}
	return strings.Join(out, ", ")
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
