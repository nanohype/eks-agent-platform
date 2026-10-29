/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

func TestEvalSuite_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	es := &governancev1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: governancev1alpha1.EvalSuiteSpec{
			PlatformRef:   commonv1alpha1.LocalRef{Name: "conformance-platform"},
			AgentFleetRef: commonv1alpha1.LocalRef{Name: "conformance-fleet"},
			Schedule:      "0 6 * * *",
			PassThreshold: "0.85",
			Cases: []governancev1alpha1.EvalCase{
				{Name: "smoke", Input: "pong?", ExpectContains: []string{"pong"}, MaxLatencyMs: 5000},
			},
		},
	}

	mustCreate(ctx, t, es)

	var got governancev1alpha1.EvalSuite
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: es.Name, Namespace: testNs}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.PassThreshold != "0.85" {
		t.Errorf("passThreshold: got %q want 0.85", got.Spec.PassThreshold)
	}
}

func TestEvalSuite_RejectsInvalidPassThreshold(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	// Fill every other constraint so the only thing the API server can be
	// rejecting on is the passThreshold pattern. Otherwise this test would
	// pass for the wrong reason if a future required-field is added to the
	// EvalSuite schema.
	es := &governancev1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: governancev1alpha1.EvalSuiteSpec{
			PlatformRef:   commonv1alpha1.LocalRef{Name: "x"},
			AgentFleetRef: commonv1alpha1.LocalRef{Name: "y"},
			PassThreshold: "1.5", // out of 0..1 range — the field under test
			Cases: []governancev1alpha1.EvalCase{
				{Name: "smoke", Input: "x", ExpectContains: []string{"x"}},
			},
		},
	}

	err := k8sClient.Create(ctx, es)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, es) })
		t.Fatalf("expected validation error for passThreshold=1.5, got nil")
	}
}

func TestEvalSuite_RejectsCasesPlusCasesFromManifest(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	es := &governancev1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName(t, "e"), Namespace: testNs},
		Spec: governancev1alpha1.EvalSuiteSpec{
			PlatformRef:   commonv1alpha1.LocalRef{Name: "x"},
			AgentFleetRef: commonv1alpha1.LocalRef{Name: "y"},
			PassThreshold: "0.85",
			// A name the casesFromManifest pattern admits, so the CEL rule is
			// the only thing left to refuse the object.
			CasesFromManifest: "smoke.json",
			Cases: []governancev1alpha1.EvalCase{
				{Name: "inline", Input: "x", ExpectContains: []string{"x"}},
			},
		},
	}

	err := k8sClient.Create(ctx, es)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, es) })
		t.Fatalf("expected validation error for both casesFromManifest AND cases set, got nil (CEL XValidation should fire)")
	}
	if !strings.Contains(err.Error(), "cases and casesFromManifest are mutually exclusive") {
		t.Fatalf("rejected, but not by the mutual-exclusion rule: %v", err)
	}
}

// TestEvalSuite_CasesFromManifestIsABareFileName holds casesFromManifest to a
// bare file name. The eval run reads <platform>/manifests/<casesFromManifest>,
// and the eval-runner role reads every Platform's manifests, so a value
// carrying a path, a URI or a parent segment would point a suite at cases
// outside its own Platform's prefix.
//
// Every refusal must name spec.casesFromManifest and nothing else: the rest of
// the object is valid, so a refusal anywhere else is a test passing for a
// reason it does not state.
func TestEvalSuite_CasesFromManifestIsABareFileName(t *testing.T) {
	ctx := context.Background()
	ensureNs(ctx, t)

	cases := []struct {
		name, manifest string
		accept         bool
	}{
		{"plain name", "smoke.json", true},
		{"one character", "a.json", true},
		{"leading digit", "0.json", true},
		{"hyphen", "nightly-v2.json", true},
		{"mixed case, underscore and single dots", "Support_Suite.2026-10.json", true},

		{"s3 URI into another bucket", "s3://elsewhere/acme/manifests/smoke.json", false},
		{"s3 URI into another platform's prefix", "s3://reports/another-platform/manifests/smoke.json", false},
		{"https URL", "https://example.com/smoke.json", false},
		{"nested path", "another-platform/manifests/smoke.json", false},
		{"parent segment", "../another-platform/manifests/smoke.json", false},
		{"bare parent segment", "..", false},
		{"two dots together", "smoke..json", false},
		{"absolute path", "/smoke.json", false},
		{"backslash separator", `manifests\smoke.json`, false},
		{"percent-encoded parent segment", "%2e%2e%2fsmoke.json", false},
		{"leading dot", ".smoke.json", false},
		{"leading hyphen", "-smoke.json", false},
		{"not json", "smoke.yaml", false},
		{"no extension", "smoke", false},
		{"query string", "smoke.json?versionId=1", false},
		{"space", "smoke test.json", false},
		{"trailing newline", "smoke.json\n", false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es := &governancev1alpha1.EvalSuite{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("eval-cfm-%d", i), Namespace: testNs},
				Spec: governancev1alpha1.EvalSuiteSpec{
					PlatformRef:       commonv1alpha1.LocalRef{Name: "x"},
					AgentFleetRef:     commonv1alpha1.LocalRef{Name: "y"},
					PassThreshold:     "0.85",
					CasesFromManifest: tc.manifest,
				},
			}

			if tc.accept {
				mustCreate(ctx, t, es)
				var got governancev1alpha1.EvalSuite
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: es.Name, Namespace: testNs}, &got); err != nil {
					t.Fatalf("get: %v", err)
				}
				if got.Spec.CasesFromManifest != tc.manifest {
					t.Errorf("casesFromManifest: got %q want %q", got.Spec.CasesFromManifest, tc.manifest)
				}
				return
			}

			err := k8sClient.Create(ctx, es)
			if err == nil {
				t.Cleanup(func() { _ = k8sClient.Delete(ctx, es) })
				t.Fatalf("casesFromManifest %q was admitted; it names something other than a file under "+
					"the Platform's own manifests/ prefix", tc.manifest)
			}
			assertRefusedOnlyOn(t, err, "spec.casesFromManifest")
		})
	}
}

// assertRefusedOnlyOn fails unless err is the API server refusing the object
// as invalid, with every cause on field.
func assertRefusedOnlyOn(t *testing.T, err error, field string) {
	t.Helper()
	var status apierrors.APIStatus
	if !apierrors.IsInvalid(err) || !errors.As(err, &status) || status.Status().Details == nil {
		t.Fatalf("want an Invalid refusal naming %s, got: %v", field, err)
	}
	causes := status.Status().Details.Causes
	if len(causes) == 0 {
		t.Fatalf("the refusal names no field, want %s: %v", field, err)
	}
	for _, c := range causes {
		if c.Field != field {
			t.Errorf("the refusal names %s (%s), want only %s", c.Field, c.Message, field)
		}
	}
}
