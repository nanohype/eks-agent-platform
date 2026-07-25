/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package agentctl

import (
	"strings"
	"testing"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// A scaffold is written to disk or piped straight to kubectl, so there is no
// admission feedback between the flags and the apply. Every rule below therefore
// has to hold here: the cases mirror the CRD's CEL rules, patterns and item
// caps, the two AWS naming limits the CRD's own ceiling does not cover, and the
// two cross-field semantics no schema can express.
func TestParseVocabulary_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		flags    VocabularyFlags
		want     string
	}{{
		name:  "datastore without a name",
		flags: VocabularyFlags{Datastores: []string{"kind=queue"}},
		want:  "name= is required",
	}, {
		name:  "datastore without a kind",
		flags: VocabularyFlags{Datastores: []string{"name=work"}},
		want:  "kind= is required",
	}, {
		name:  "unknown kind",
		flags: VocabularyFlags{Datastores: []string{"name=store,kind=documentDb"}},
		want:  `unknown kind "documentDb"`,
	}, {
		name:  "not key=value",
		flags: VocabularyFlags{Datastores: []string{"name=work,queue"}},
		want:  "is not key=value",
	}, {
		name:  "unknown key",
		flags: VocabularyFlags{Datastores: []string{"name=work,kind=queue,visibilityTimeout=30"}},
		want:  `unknown key "visibilityTimeout"`,
	}, {
		name:  "duplicate key",
		flags: VocabularyFlags{Datastores: []string{"name=work,kind=queue,kind=cache"}},
		want:  `key "kind" given twice`,
	}, {
		// CRD pattern on DatastoreSpec.name.
		name:  "name is not an RFC-1123 label",
		flags: VocabularyFlags{Datastores: []string{"name=Work_Queue,kind=queue"}},
		want:  "must be a short RFC-1123 label",
	}, {
		name:  "name over 18 characters",
		flags: VocabularyFlags{Datastores: []string{"name=nineteen-chars-abcd,kind=stream"}},
		want:  "must be a short RFC-1123 label",
	}, {
		// CRD CEL: metadata.name + datastore name <= 28.
		name:     "composed name over the 28-character budget",
		platform: "platform-with-a-longish-name",
		flags:    VocabularyFlags{Datastores: []string{"name=ledger,kind=stream"}},
		want:     "over the 28-character budget",
	}, {
		// Tighter than the CRD: ElastiCache caps a replication-group id at 40
		// including the longest environment token, so a cache at exactly 28
		// passes admission and then fails the tenant-substrate module.
		name:     "cache name over the 27-character budget",
		platform: "twenty-two-chars-here",
		flags:    VocabularyFlags{Datastores: []string{"name=hotcache,kind=cache"}},
		want:     "over the 27-character budget for kind=cache",
	}, {
		name:  "duplicate datastore names",
		flags: VocabularyFlags{Datastores: []string{"name=work,kind=queue", "name=work,kind=stream"}},
		want:  "declared twice",
	}, {
		name:  "bad deletion policy",
		flags: VocabularyFlags{Datastores: []string{"name=work,kind=queue,deletionPolicy=Orphan"}},
		want:  "deletionPolicy must be Retain or Delete",
	}, {
		// CRD CEL: kind=keyValue requires the keyValue block, whose partitionKey
		// is a required field.
		name:  "keyValue without a partition key",
		flags: VocabularyFlags{Datastores: []string{"name=sessions,kind=keyValue"}},
		want:  "a DynamoDB table has no default partition key",
	}, {
		name:  "partition key without a type",
		flags: VocabularyFlags{Datastores: []string{"name=sessions,kind=keyValue,partitionKey=sessionId"}},
		want:  "must be <attr>:<type>",
	}, {
		name:  "partition key with an unknown type",
		flags: VocabularyFlags{Datastores: []string{"name=sessions,kind=keyValue,partitionKey=sessionId:string"}},
		want:  `type must be S, N, or B, got "string"`,
	}, {
		// The CRD's config-block-matches-kind rule, expressed in flag terms.
		name:  "key schema on a non-keyValue kind",
		flags: VocabularyFlags{Datastores: []string{"name=docs,kind=objectStore,partitionKey=docId:S"}},
		want:  "only apply to kind=keyValue",
	}, {
		// CRD enum on Capability.
		name:  "unknown capability",
		flags: VocabularyFlags{Capabilities: []string{"lambdaInvoke"}},
		want:  `unknown capability "lambdaInvoke"`,
	}, {
		// CRD MaxItems=8.
		name: "over the capability cap",
		flags: VocabularyFlags{Capabilities: []string{
			"ses", "ses", "ses", "ses", "ses", "ses", "ses", "ses", "ses",
		}},
		want: "at most 8 capabilities",
	}, {
		// Not a CRD rule — the operator scopes the minted scheduler-invoke role's
		// SendMessage to the tenant's own queues, so the capability without a
		// queue creates a role carrying no grant at all.
		name:  "scheduler capability with no queue to send to",
		flags: VocabularyFlags{Capabilities: []string{"eventBridgeScheduler"}},
		want:  "needs at least one kind=queue datastore",
	}, {
		// CRD pattern on DirectSecretReads items.
		name:  "secret read is not a Secrets Manager name",
		flags: VocabularyFlags{DirectSecretReads: []string{"has spaces"}},
		want:  "is not a Secrets Manager name",
	}, {
		// Not a CRD rule — the CRD pattern permits '/', so a full path passes
		// admission and the operator grants on the prefix twice.
		name:     "secret read already carries the platform prefix",
		platform: "docs-rag",
		flags:    VocabularyFlags{DirectSecretReads: []string{"docs-rag/production/vendor/key"}},
		want:     "entries are prefix-relative",
	}, {
		// CRD MaxItems=16.
		name: "over the secret-read cap",
		flags: VocabularyFlags{DirectSecretReads: []string{
			"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q",
		}},
		want: "at most 16 direct secret reads",
	}, {
		name:  "empty attribution operator",
		flags: VocabularyFlags{Operators: []string{"  "}},
		want:  "must not be empty",
	}, {
		// The same string binds the AWS SourceIdentity and the impersonate
		// ClusterRole's resourceNames, so a case mismatch attributes to nobody.
		name:  "mixed-case attribution operator",
		flags: VocabularyFlags{Operators: []string{"Operator@Example.com"}},
		want:  "must be lowercase",
	}, {
		// CRD Minimum=900.
		name:  "session lifetime under the floor",
		flags: VocabularyFlags{Operators: []string{"op@example.com"}, SessionRoleMaxSecs: 60},
		want:  "must be between 900 and 43200",
	}, {
		// CRD Maximum=43200.
		name:  "session lifetime over the ceiling",
		flags: VocabularyFlags{Operators: []string{"op@example.com"}, SessionRoleMaxSecs: 90000},
		want:  "must be between 900 and 43200",
	}, {
		name:  "session lifetime without attribution",
		flags: VocabularyFlags{SessionRoleMaxSecs: 3600},
		want:  "no effect without --attribution-operator",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			platform := tc.platform
			if platform == "" {
				platform = "cover"
			}
			_, err := ParseVocabulary(platform, tc.flags)
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the reason.\n  want substring: %s\n  got: %v", tc.want, err)
			}
		})
	}
}

func TestParseVocabulary_ParsesEveryKind(t *testing.T) {
	got, err := ParseVocabulary("cover", VocabularyFlags{
		Datastores: []string{
			"name=ledger,kind=relational",
			"name=sessions,kind=keyValue,partitionKey=sessionId:S,sortKey=createdAt:N",
			"name=docs,kind=objectStore,deletionPolicy=Delete",
			"name=work,kind=queue",
			"name=hot,kind=cache",
			"name=events,kind=stream",
		},
		Capabilities:      []string{"ses", "eventBridgeScheduler"},
		DirectSecretReads: []string{"oncall/webhook-hmac"},
		Operators:         []string{"operator@example.com"},
	})
	if err != nil {
		t.Fatalf("ParseVocabulary: %v", err)
	}

	wantKinds := []platformv1alpha1.DatastoreKind{
		platformv1alpha1.DatastoreRelational,
		platformv1alpha1.DatastoreKeyValue,
		platformv1alpha1.DatastoreObjectStore,
		platformv1alpha1.DatastoreQueue,
		platformv1alpha1.DatastoreCache,
		platformv1alpha1.DatastoreStream,
	}
	if len(got.Datastores) != len(wantKinds) {
		t.Fatalf("want %d datastores, got %d", len(wantKinds), len(got.Datastores))
	}
	for i, want := range wantKinds {
		if got.Datastores[i].Kind != want {
			t.Errorf("datastore %d kind: got %q want %q", i, got.Datastores[i].Kind, want)
		}
	}

	// Flag order is declaration order: the emitted list has to be stable so a
	// re-scaffold produces the same bytes.
	if got.Datastores[0].Name != "ledger" || got.Datastores[5].Name != "events" {
		t.Errorf("declaration order not preserved: %q … %q", got.Datastores[0].Name, got.Datastores[5].Name)
	}

	// Only the keyValue store carries a config block — every other kind takes
	// the CRD's defaults rather than having them restated by the scaffolder.
	kv := got.Datastores[1]
	if kv.KeyValue == nil {
		t.Fatal("keyValue datastore has no keyValue block")
	}
	if kv.KeyValue.PartitionKey.Name != "sessionId" || kv.KeyValue.PartitionKey.Type != "S" {
		t.Errorf("partition key: got %+v", kv.KeyValue.PartitionKey)
	}
	if kv.KeyValue.SortKey == nil || kv.KeyValue.SortKey.Type != "N" {
		t.Errorf("sort key: got %+v", kv.KeyValue.SortKey)
	}
	if got.Datastores[0].Relational != nil || got.Datastores[3].Queue != nil || got.Datastores[4].Cache != nil {
		t.Error("a kind whose block was not configured should carry no block")
	}
	if got.Datastores[2].DeletionPolicy != "Delete" {
		t.Errorf("deletionPolicy: got %q want Delete", got.Datastores[2].DeletionPolicy)
	}

	if att := got.Attribution(); att == nil {
		t.Error("attribution should be set when an operator is named")
	} else if *att.SessionRoleMaxDurationSeconds != 3600 {
		t.Errorf("session lifetime should default to 3600, got %d", *att.SessionRoleMaxDurationSeconds)
	}
	if got.Empty() {
		t.Error("Empty() should be false once anything is declared")
	}
}

// Attribution is opt-in by content, not by flag: the CRD requires at least one
// operator whenever the block is present, so no operators must mean no block —
// not an empty one that fails admission.
func TestParseVocabulary_NoVocabularyIsEmpty(t *testing.T) {
	got, err := ParseVocabulary("cover", VocabularyFlags{})
	if err != nil {
		t.Fatalf("ParseVocabulary: %v", err)
	}
	if !got.Empty() {
		t.Error("Empty() should be true when nothing is declared")
	}
	if got.Attribution() != nil {
		t.Error("Attribution() must be nil with no operators, so the block is omitted entirely")
	}
}

func TestParseVocabulary_HonorsSessionLifetime(t *testing.T) {
	got, err := ParseVocabulary("cover", VocabularyFlags{
		Operators:          []string{"operator@example.com"},
		SessionRoleMaxSecs: 7200,
	})
	if err != nil {
		t.Fatalf("ParseVocabulary: %v", err)
	}
	if *got.Attribution().SessionRoleMaxDurationSeconds != 7200 {
		t.Errorf("session lifetime: got %d want 7200", *got.Attribution().SessionRoleMaxDurationSeconds)
	}
}
