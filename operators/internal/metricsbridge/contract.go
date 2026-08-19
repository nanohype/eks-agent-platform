/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package metricsbridge holds the wire contract between the metrics-shim
// binary and the SandboxPool reconciler that deploys it.
//
// The two halves never reference each other at runtime — the shim serves HTTP
// and KEDA's metrics-api scaler reads it, configured by a ScaledObject the
// reconciler renders. Nothing in Go related them, so both the port and the JSON
// key were written twice, in different packages, in different forms: an address
// string here and an int there, a map literal here and a metadata string there.
//
// Either pair drifting is silent. KEDA scrapes and gets a connection refused,
// or scrapes successfully and finds no such key; either way the scaler reports
// no metric, the pool holds at minReplicas, and its work queue grows beside a
// ScaledObject that looks correctly configured. Nothing errors and no status
// says so.
//
// Sharing the symbols is what makes that drift impossible rather than merely
// detectable — a test can only check the two sides still agree, whereas one
// constant cannot disagree with itself.
package metricsbridge

import (
	"encoding/json"
	"fmt"
	"io"
)

// Port is the shim's listen port, the port its Service targets, and the port
// the bridge NetworkPolicy admits KEDA to.
const Port = 8080

// ListenAddr is Port in the form net/http wants.
var ListenAddr = fmt.Sprintf(":%d", Port)

// DepthKey is the JSON object key the shim writes the queue depth under, and
// the value the ScaledObject's metrics-api trigger passes as valueLocation —
// which is how KEDA knows which field of the response body to read.
const DepthKey = "depth"

// WriteDepth serves the depth in the shape the scaler expects. It is the only
// place the response body is constructed, so DepthKey cannot be spelled one way
// here and another way in what the reconciler tells KEDA to look for.
func WriteDepth(w io.Writer, depth int64) error {
	return json.NewEncoder(w).Encode(map[string]int64{DepthKey: depth})
}
