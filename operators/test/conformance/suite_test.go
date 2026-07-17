/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package conformance hosts envtest-based round-trip tests against the
// CRDs in operators/config/crd/bases/. Tests prove: scheme registers,
// CRs Create→Get→Patch→Delete cleanly through the API server, status
// subresources accept writes, and validation rejections happen where
// expected.
//
// These run on every PR via `make test` / `go test ./...` once
// KUBEBUILDER_ASSETS is set (the Makefile does this via setup-envtest).
//
// Test isolation:
//
//   - Each test derives a unique resource-name suffix from t.Name() via
//     the uniqueName() helper. This prevents name collisions when adding
//     new tests in the suite.
//   - Tests register t.Cleanup that deletes the resources they create.
//   - For `go test -count=N>1` or parallel-test scenarios where the same
//     test function runs twice in the same envtest instance, cleanup
//     uses ForegroundDeletion via the t.Cleanup so subsequent Creates
//     don't race the API server's eventual removal of the prior object.
package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    *runtime.Scheme
)

// uniqueName derives a Kubernetes-valid (RFC 1123 subdomain label) resource
// name from t.Name() + suffix so test resources don't collide across the
// suite. Lowercase, hyphenated, 63-char-capped.
func uniqueName(t interface{ Name() string }, suffix string) string {
	n := strings.ToLower(t.Name())
	n = strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(n)
	if suffix != "" {
		n = n + "-" + suffix
	}
	if len(n) > 63 {
		n = n[:63]
	}
	return strings.Trim(n, "-")
}

// mustCreate registers t.Cleanup *before* Create so the cleanup runs even if
// Create panics or the t.Fatalf path triggers — eliminates the test-leak
// window where a Create that succeeds, then a t.Fatalf that fires before
// the t.Cleanup registration line, would leave a stale object in the
// shared `conformance` namespace and break `go test -count=N>1`.
// Delete on a never-created object NotFound's, which is ignored.
func mustCreate(ctx context.Context, t *testing.T, obj client.Object) {
	t.Helper()
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, obj) })
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create %T: %v", obj, err)
	}
}

func TestMain(m *testing.M) {
	scheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(governancev1alpha1.AddToScheme(scheme))

	testEnv = &envtest.Environment{
		// Operator CRDs + minimal ArgoCD CRDs (Application/AppProject) so the
		// vcluster tier's ArgoCD declarations and the AppProject destination
		// scoping are exercised against a real API server.
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("testdata", "argocd-crds.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic("envtest start: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = testEnv.Stop()
		panic("client.New: " + err.Error())
	}

	// The AppProject / vcluster Application land in the argocd namespace; create
	// it once so ensureAppProject / ensureVClusterApplication have somewhere to
	// write now that the ArgoCD CRDs are installed.
	if err := k8sClient.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "argocd"},
	}); err != nil {
		_ = testEnv.Stop()
		panic("create argocd namespace: " + err.Error())
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		// Don't override the test result with a teardown error, but surface it.
		println("envtest stop:", err.Error())
	}
	os.Exit(code)
}
