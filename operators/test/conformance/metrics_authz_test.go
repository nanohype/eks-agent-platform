/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package conformance

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
)

// TestMetricsEndpointRejectsUnauthenticated proves the security property behind
// --metrics-secure: the controller-runtime authentication+authorization filter
// the operator installs on its metrics server rejects a scrape that carries no
// bearer token, at the HTTP layer — the metrics never reach the wrapped handler.
// Built against the envtest API server so TokenReview / SubjectAccessReview are
// real, this is the runtime counterpart to the cmd-package wiring test.
func TestMetricsEndpointRejectsUnauthenticated(t *testing.T) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("http client for rest config: %v", err)
	}
	filter, err := filters.WithAuthenticationAndAuthorization(cfg, httpClient)
	if err != nil {
		t.Fatalf("build authn/authz filter: %v", err)
	}

	served := false
	wrapped, err := filter(logr.Discard(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("agent_platform_up 1\n"))
	}))
	if err != nil {
		t.Fatalf("wrap metrics handler: %v", err)
	}

	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics") //nolint:noctx // test-local one-shot request
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauthenticated scrape returned 200; endpoint must reject it (handler served=%v)", served)
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated scrape: got status %d, want 401 or 403", resp.StatusCode)
	}
	if served {
		t.Fatal("wrapped metrics handler ran for an unauthenticated request; filter did not gate it")
	}
}
