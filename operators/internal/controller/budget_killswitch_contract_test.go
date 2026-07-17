/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The kill-switch spans two languages that never share a symbol: the Go
// reconciler publishes an EventBridge event, and a terraform-managed rule
// matches it. EventBridge matching is exact — if the Go constants and the
// terraform event_pattern ever disagree on the source, detail-type, or
// severity, the suspension state machine silently never fires and the
// reconciler records a false success. This test parses the terraform
// event_pattern and asserts it equals the Go constants, so drift on either
// side of the seam fails the build instead of the production kill-switch.

var (
	tfSourceRE     = regexp.MustCompile(`(?m)^\s*source\s*=\s*\[\s*"([^"]+)"\s*\]`)
	tfDetailTypeRE = regexp.MustCompile(`(?m)^\s*"detail-type"\s*=\s*\[\s*"([^"]+)"\s*\]`)
	tfSeverityRE   = regexp.MustCompile(`(?m)^\s*severity\s*=\s*\[\s*"([^"]+)"\s*\]`)
)

func TestKillSwitchEventContract(t *testing.T) {
	// Package dir → repo root is three levels up.
	path := filepath.Join("..", "..", "..", "terraform", "components", "kill-switch", "main.tf")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read terraform kill-switch config at %s: %v", path, err)
	}
	tf := string(raw)

	cases := []struct {
		field string
		re    *regexp.Regexp
		want  string
	}{
		{"source", tfSourceRE, budgetEventSource},
		{"detail-type", tfDetailTypeRE, budgetEventDetailType},
		{"detail.severity", tfSeverityRE, budgetEventSeverity},
	}
	for _, c := range cases {
		m := c.re.FindStringSubmatch(tf)
		if m == nil {
			t.Fatalf("could not find %q in the terraform event_pattern; the contract regex or the terraform layout drifted (%s)", c.field, path)
		}
		if got := m[1]; got != c.want {
			t.Errorf("kill-switch %s: terraform event_pattern has %q, Go constant has %q — the EventBridge match is now dead on one side; align both", c.field, got, c.want)
		}
	}
}
