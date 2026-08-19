/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package metricsbridge

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestWriteDepthUsesTheDeclaredKey reads the body the shim serves and confirms
// it carries DepthKey — the same symbol the ScaledObject's valueLocation is
// built from.
//
// With both sides sharing the constant the two can no longer disagree, so this
// is not guarding the drift the package exists to prevent. What it guards is the
// encoding: WriteDepth could serve the right key nested, or as a string, or
// alongside others, and KEDA's metrics-api scaler reads a flat numeric field.
func TestWriteDepthUsesTheDeclaredKey(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteDepth(&buf, 42); err != nil {
		t.Fatalf("WriteDepth: %v", err)
	}

	var got map[string]json.Number
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("body is not a flat JSON object: %v (body %q)", err, buf.String())
	}
	if len(got) != 1 {
		t.Errorf("body has %d keys, want exactly one — the scaler reads a single field and extra keys "+
			"are unread payload: %v", len(got), got)
	}
	v, ok := got[DepthKey]
	if !ok {
		t.Fatalf("body has no %q key: %v", DepthKey, got)
	}
	if v.String() != "42" {
		t.Errorf("%s = %s, want 42", DepthKey, v.String())
	}

	// Numeric, not quoted: valueLocation resolves to a value the scaler parses
	// as a number, and a string here fails at scrape time rather than here.
	if strings.Contains(buf.String(), `"42"`) {
		t.Errorf("depth is encoded as a string in %q — the scaler expects a number", buf.String())
	}
}

// TestListenAddrMatchesPort keeps the two representations of one port in step.
// ListenAddr is derived from Port, so this cannot fail today; it exists so that
// replacing the derivation with a literal — the obvious "simplification" —
// fails here rather than at the next scrape.
func TestListenAddrMatchesPort(t *testing.T) {
	if want := ":8080"; ListenAddr != want {
		t.Errorf("ListenAddr = %q, want %q", ListenAddr, want)
	}
	if Port != 8080 {
		t.Errorf("Port = %d, want 8080 — the NetworkPolicy, the Service and the shim all take it from here", Port)
	}
	if !strings.HasSuffix(ListenAddr, "8080") {
		t.Errorf("ListenAddr %q does not carry Port %d", ListenAddr, Port)
	}
}
