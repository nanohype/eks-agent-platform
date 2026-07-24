/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
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
//
// The parse is scoped to the named rule (readEventRule, in
// slo_killswitch_contract_test.go) because the bus now carries more than one
// rule. A whole-file regex would take the first match it found, so a rule added
// above this one would silently retarget the assertion and keep the test green
// while it checked the wrong thing.

func TestKillSwitchEventContract(t *testing.T) {
	got := readEventRule(t, "main.tf", "breach")

	cases := []struct {
		field string
		got   string
		want  string
	}{
		{"source", got.source, budgetEventSource},
		{"detail-type", got.detailType, budgetEventDetailType},
		{"detail.severity", got.severity, budgetEventSeverity},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("kill-switch %s: terraform event_pattern has %q, Go constant has %q — the EventBridge match is now dead on one side; align both", c.field, c.got, c.want)
		}
	}
}
