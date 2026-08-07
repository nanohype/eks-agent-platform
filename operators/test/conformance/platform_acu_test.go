/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The ACU bounds are the only thing standing between a Platform CR and an
// Aurora scaling configuration. That was worth pinning against a real apiserver
// rather than reading the generated YAML, because for as long as the field
// existed its doc comment claimed the range and the maxACU >= minACU relation
// were "enforced at the tenant-substrate module's variable boundary" — and that
// module has eleven validation blocks, none of which mentions ACU. The pattern
// was the only check there was, and it admitted "999" (past Aurora's 256 cap)
// while rejecting "0" (the auto-pause floor), in a field whose default nobody
// had reason to change.
//
// Two of these cases are CEL, which is evaluated by the apiserver and by
// nothing else — a unit test cannot reach them at all.

func acuPlatform(name, minACU, maxACU, engine string) *platformv1alpha1.Platform {
	return &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{{
				Name: "db",
				Kind: platformv1alpha1.DatastoreRelational,
				Relational: &platformv1alpha1.RelationalConfig{
					MinACU:        minACU,
					MaxACU:        maxACU,
					EngineVersion: engine,
				},
			}},
		},
	}
}

func TestPlatform_ACUBounds(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	cases := []struct {
		name    string
		minACU  string
		maxACU  string
		engine  string
		accept  bool
		because string
	}{
		{"auto-pause floor", "0", "8", "16.6", true,
			"0 is scale-to-zero and must be expressible — the old pattern could not say it"},
		{"half-ACU floor", "0.5", "8", "16.6", true, "the documented young/light default"},
		{"whole ACU", "2", "16", "16.6", true, "ordinary case"},
		{"half step", "1.5", "2.5", "16.6", true, "0.5 steps are legal throughout the range"},
		{"ceiling", "0.5", "256", "16.6", true, "256 ACU is Aurora's documented maximum"},
		{"floor equals ceiling", "4", "4", "16.6", true, "a fixed-capacity cluster is legal"},

		{"past Aurora's cap", "0.5", "257", "16.6", false,
			"the old pattern admitted up to 999; AWS rejects anything over 256 at apply"},
		{"257 as a floor", "257", "512", "16.6", false, "same cap on the floor"},
		{"non-half step", "1.25", "8", "16.6", false, "capacity moves in 0.5 increments only"},
		{"256.5", "0.5", "256.5", "16.6", false, "there is no half step above the cap"},
		{"zero ceiling", "0", "0", "16.6", false,
			"a ceiling of zero leaves no capacity to scale into; only the floor may be 0"},
		{"negative", "-1", "8", "16.6", false, "not a capacity"},

		{"ceiling below floor", "8", "0.5", "16.6", false,
			"maxACU >= minACU — CEL, unreachable from a unit test"},
		{"ceiling just below floor", "2.5", "2", "16.6", false, "the relation is strict about halves too"},

		{"auto-pause on an engine too old", "0", "8", "15.4", false,
			"scale-to-zero needs Aurora PostgreSQL 16.3+; 15.4 would silently never pause"},
		{"non-zero floor on an old engine", "0.5", "8", "15.4", true,
			"the engine bound applies only to the auto-pause floor"},
		{"auto-pause on a future major", "0", "8", "17.2", true, "17.x is past the 16.3 bar"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := acuPlatform(fmt.Sprintf("pds-acu-%d", i), tc.minACU, tc.maxACU, tc.engine)
			err := k8sClient.Create(ctx, p)
			if err == nil {
				t.Cleanup(func() { _ = k8sClient.Delete(ctx, p) })
			}
			switch {
			case tc.accept && err != nil:
				t.Fatalf("minACU=%q maxACU=%q engineVersion=%q was rejected but must be accepted (%s): %v",
					tc.minACU, tc.maxACU, tc.engine, tc.because, err)
			case !tc.accept && err == nil:
				t.Fatalf("minACU=%q maxACU=%q engineVersion=%q was accepted but must be rejected (%s)",
					tc.minACU, tc.maxACU, tc.engine, tc.because)
			}
		})
	}
}

// TestPlatform_ACUDefaults proves the defaults an omitted block takes are the
// ones the type documents, so "0.5–8" in the doc comment is a fact about the
// served schema rather than a description of intent.
func TestPlatform_ACUDefaults(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	p := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "pds-acu-defaults", Namespace: testNs},
		Spec: platformv1alpha1.PlatformSpec{
			Persona:  "ops",
			Tenant:   "conformance",
			Budget:   platformv1alpha1.BudgetRef{Name: "x"},
			Identity: platformv1alpha1.IdentitySpec{AllowedModelFamilies: []string{"anthropic"}},
			Datastores: []platformv1alpha1.DatastoreSpec{{
				Name:       "db",
				Kind:       platformv1alpha1.DatastoreRelational,
				Relational: &platformv1alpha1.RelationalConfig{},
			}},
		},
	}
	mustCreate(ctx, t, p)

	got := findDatastore(p.Spec.Datastores, "db")
	if got == nil || got.Relational == nil {
		t.Fatal("relational block missing after create")
	}
	if got.Relational.MinACU != "0.5" || got.Relational.MaxACU != "8" {
		t.Errorf("ACU defaults: got %s–%s, want 0.5–8", got.Relational.MinACU, got.Relational.MaxACU)
	}
	if got.Relational.EngineVersion != "16.6" {
		t.Errorf("engineVersion default: got %q want 16.6", got.Relational.EngineVersion)
	}
}
