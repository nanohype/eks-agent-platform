/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import "testing"

// TestMetricsServerOptions_SecureEnablesAuthnAuthz pins the security wiring: in
// secure mode the metrics server must serve over HTTPS and install the
// authentication+authorization filter, so an unauthenticated scrape is rejected
// at the HTTP layer (not only by the NetworkPolicy). Deleting either line
// regresses the STRIDE info-disclosure fix and fails here.
func TestMetricsServerOptions_SecureEnablesAuthnAuthz(t *testing.T) {
	opts := metricsServerOptions(true, ":8080")
	if opts.BindAddress != ":8080" {
		t.Errorf("bind address: got %q want %q", opts.BindAddress, ":8080")
	}
	if !opts.SecureServing {
		t.Error("secure metrics must enable SecureServing (HTTPS)")
	}
	if opts.FilterProvider == nil {
		t.Error("secure metrics must set the authn/authz FilterProvider so unauthenticated scrapes are rejected")
	}
}

// TestMetricsServerOptions_InsecureIsPlainHTTP covers the local/dev escape hatch:
// no TLS, no filter — usable where no kube-apiserver is reachable to review
// scrape tokens.
func TestMetricsServerOptions_InsecureIsPlainHTTP(t *testing.T) {
	opts := metricsServerOptions(false, ":8080")
	if opts.SecureServing {
		t.Error("insecure metrics must not enable SecureServing")
	}
	if opts.FilterProvider != nil {
		t.Error("insecure metrics must not set a FilterProvider")
	}
}
