/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// findDatastore returns the spec entry with the given name, or nil.
func findDatastore(ds []platformv1alpha1.DatastoreSpec, name string) *platformv1alpha1.DatastoreSpec {
	for i := range ds {
		if ds[i].Name == name {
			return &ds[i]
		}
	}
	return nil
}

// TestPlatform_DatastoresValidVocabulary proves the datastore vocabulary
// round-trips through the API server: one entry of every kind is accepted, a
// present-but-empty config block is filled with the young/light defaults, a
// field-level default (deletionPolicy) applies even when the block is omitted,
// and a kind whose block is omitted (objectStore) is accepted. This is the
// positive path for the whole Target-1 vocabulary.
func TestPlatform_DatastoresValidVocabulary(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-vocab", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "eng",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{
				// present-but-empty block: server-side defaults must fill it.
				{Name: "db", Kind: platformv1alpha1.DatastoreRelational, Relational: &platformv1alpha1.RelationalConfig{}},
				// keyValue requires its block (mandatory partition key).
				{Name: "kv", Kind: platformv1alpha1.DatastoreKeyValue, KeyValue: &platformv1alpha1.KeyValueConfig{
					PartitionKey: platformv1alpha1.AttributeSchema{Name: "pk", Type: "S"},
				}},
				// block omitted: the tofu module applies defaults; only the
				// field-level deletionPolicy default applies at the CRD.
				{Name: "obj", Kind: platformv1alpha1.DatastoreObjectStore},
				{Name: "q", Kind: platformv1alpha1.DatastoreQueue},
				{Name: "ca", Kind: platformv1alpha1.DatastoreCache},
				// stream carries no block.
				{Name: "st", Kind: platformv1alpha1.DatastoreStream},
			},
		},
	}
	mustCreate(ctx, t, p)

	var got platformv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Spec.Datastores) != 6 {
		t.Fatalf("datastores round-trip: got %d entries want 6", len(got.Spec.Datastores))
	}

	db := findDatastore(got.Spec.Datastores, "db")
	if db == nil || db.Relational == nil {
		t.Fatalf("db entry or its relational block missing after round-trip")
	}
	if db.DeletionPolicy != "Retain" {
		t.Errorf("db deletionPolicy default: got %q want Retain", db.DeletionPolicy)
	}
	if db.Relational.MinACU != "0.5" || db.Relational.MaxACU != "8" {
		t.Errorf("relational ACU defaults: got min=%q max=%q want 0.5/8", db.Relational.MinACU, db.Relational.MaxACU)
	}
	if db.Relational.EngineVersion != "16.6" {
		t.Errorf("relational engineVersion default: got %q want 16.6", db.Relational.EngineVersion)
	}
	if db.Relational.BackupRetentionDays != 7 {
		t.Errorf("relational backupRetentionDays default: got %d want 7", db.Relational.BackupRetentionDays)
	}
	if db.Relational.DeletionProtection == nil || !*db.Relational.DeletionProtection {
		t.Errorf("relational deletionProtection default: got %v want true", db.Relational.DeletionProtection)
	}

	kv := findDatastore(got.Spec.Datastores, "kv")
	if kv == nil || kv.KeyValue == nil {
		t.Fatalf("kv entry or its keyValue block missing after round-trip")
	}
	if kv.KeyValue.BillingMode != "PAY_PER_REQUEST" {
		t.Errorf("keyValue billingMode default: got %q want PAY_PER_REQUEST", kv.KeyValue.BillingMode)
	}
	if kv.KeyValue.PointInTimeRecovery == nil || !*kv.KeyValue.PointInTimeRecovery {
		t.Errorf("keyValue pointInTimeRecovery default: got %v want true", kv.KeyValue.PointInTimeRecovery)
	}

	obj := findDatastore(got.Spec.Datastores, "obj")
	if obj == nil || obj.DeletionPolicy != "Retain" {
		t.Errorf("obj deletionPolicy default with block omitted: got %v", obj)
	}
}

// TestPlatform_DatastoreStatusSubresource proves per-datastore observed state
// (T3/(a), T6) writes through the status subresource independently of the
// top-level phase.
func TestPlatform_DatastoreStatusSubresource(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-status", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{
				{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
			},
		},
	}
	mustCreate(ctx, t, p)

	p.Status.Phase = phaseReady
	p.Status.Datastores = []platformv1alpha1.DatastoreStatus{
		{Name: "db", Kind: platformv1alpha1.DatastoreRelational, Phase: phaseProvisioning, SecretName: "db-master", Drift: []string{"engineVersion"}},
	}
	if err := k8sClient.Status().Update(ctx, p); err != nil {
		t.Fatalf("status update: %v", err)
	}

	var got platformv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get after status update: %v", err)
	}
	if got.Status.Phase != phaseReady {
		t.Errorf("top-level phase: got %q want Ready", got.Status.Phase)
	}
	if len(got.Status.Datastores) != 1 || got.Status.Datastores[0].Phase != phaseProvisioning {
		t.Fatalf("datastore status round-trip: got %+v", got.Status.Datastores)
	}
	if got.Status.Datastores[0].SecretName != "db-master" {
		t.Errorf("datastore secretName: got %q want db-master", got.Status.Datastores[0].SecretName)
	}
	if len(got.Status.Datastores[0].Drift) != 1 || got.Status.Datastores[0].Drift[0] != "engineVersion" {
		t.Errorf("datastore drift: got %v", got.Status.Datastores[0].Drift)
	}
}

// TestPlatform_RejectsMismatchedDatastoreBlock proves the kind↔block
// consistency CEL rule fires: a relational datastore carrying a keyValue block
// is rejected at admission.
func TestPlatform_RejectsMismatchedDatastoreBlock(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-mismatch", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{
				{Name: "db", Kind: platformv1alpha1.DatastoreRelational, KeyValue: &platformv1alpha1.KeyValueConfig{
					PartitionKey: platformv1alpha1.AttributeSchema{Name: "pk", Type: "S"},
				}},
			},
		},
	}
	if err := k8sClient.Create(ctx, p); err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, p) })
		t.Fatalf("expected validation error for relational kind with keyValue block, got nil (kind↔block CEL should fire)")
	}
}

// TestPlatform_RejectsKeyValueMissingBlock proves keyValue's block is required:
// a DynamoDB table has no default partition key, so an omitted block is
// rejected — unlike the other kinds, which may omit their block.
func TestPlatform_RejectsKeyValueMissingBlock(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-kvmissing", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{
				{Name: "kv", Kind: platformv1alpha1.DatastoreKeyValue},
			},
		},
	}
	if err := k8sClient.Create(ctx, p); err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, p) })
		t.Fatalf("expected validation error for keyValue kind with no block, got nil (required-block CEL should fire)")
	}
}

// TestPlatform_RejectsInvalidDatastoreKind proves the kind enum is closed: an
// unknown kind is rejected at admission.
func TestPlatform_RejectsInvalidDatastoreKind(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-badkind", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{
				{Name: "g", Kind: platformv1alpha1.DatastoreKind("graph")},
			},
		},
	}
	if err := k8sClient.Create(ctx, p); err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, p) })
		t.Fatalf("expected validation error for unknown datastore kind, got nil (kind enum should reject)")
	}
}

// TestPlatform_RejectsCombinedNameTooLong proves the IAM/S3 name-length budget
// is enforced at the CRD boundary (the acceptance criterion): a platform name
// plus a datastore name exceeding the combined budget is rejected before any
// resource is composed, so no generated name can overrun S3's 63-char ceiling.
func TestPlatform_RejectsCombinedNameTooLong(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	// "pds-combined-name" (17) + "a-very-long-store" (17) = 34 > 28.
	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-combined-name", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{
				{Name: "a-very-long-store", Kind: platformv1alpha1.DatastoreObjectStore},
			},
		},
	}
	if err := k8sClient.Create(ctx, p); err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, p) })
		t.Fatalf("expected validation error for platform+datastore name > 28 combined, got nil (name-length CEL should fire)")
	}
}

// TestPlatform_RejectsDatastoreKindChange proves a datastore's kind is
// immutable: flipping relational→keyValue (with the block swapped so only the
// kind-immutability rule is the distinguishing rejection) is denied, since
// changing kind would strand the provisioned resource.
func TestPlatform_RejectsDatastoreKindChange(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-kindswap", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{
				{Name: "d", Kind: platformv1alpha1.DatastoreRelational, Relational: &platformv1alpha1.RelationalConfig{}},
			},
		},
	}
	mustCreate(ctx, t, p)

	var got platformv1alpha1.Platform
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.Datastores[0].Kind = platformv1alpha1.DatastoreKeyValue
	got.Spec.Datastores[0].Relational = nil
	got.Spec.Datastores[0].KeyValue = &platformv1alpha1.KeyValueConfig{
		PartitionKey: platformv1alpha1.AttributeSchema{Name: "pk", Type: "S"},
	}
	if err := k8sClient.Update(ctx, &got); err == nil {
		t.Fatalf("expected validation error changing datastore kind relational→keyValue, got nil (immutability CEL should fire)")
	}
}
