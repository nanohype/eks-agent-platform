/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

// breachEntry is a well-formed entry the kill-switch bus would accept.
func breachEntry() ebtypes.PutEventsRequestEntry {
	return ebtypes.PutEventsRequestEntry{
		EventBusName: aws.String("killswitch-bus"),
		Source:       aws.String("governance.nanohype.dev/budget"),
		DetailType:   aws.String("BudgetBreach"),
		Detail:       aws.String(`{"platformId":"acme","percentOfBudget":150}`),
	}
}

func entries(n int, edit func(*ebtypes.PutEventsRequestEntry)) []ebtypes.PutEventsRequestEntry {
	out := make([]ebtypes.PutEventsRequestEntry, n)
	for i := range out {
		out[i] = breachEntry()
		if edit != nil {
			edit(&out[i])
		}
	}
	return out
}

// TestEventBridgeRefusesWhatEventBridgeRefuses: a call outside 1-10 entries
// fails whole; an entry without its Source, DetailType or a JSON-object Detail
// fails alone, counted in FailedEntryCount, while the call succeeds.
func TestEventBridgeRefusesWhatEventBridgeRefuses(t *testing.T) {
	cases := []struct {
		name    string
		entries []ebtypes.PutEventsRequestEntry
		setup   func(*awsfake.EventBridge)
		// wantCallErr is the whole-call error code; "" wants the call to succeed.
		wantCallErr string
		// wantCode is the per-entry ErrorCode of the first entry; "" wants it published.
		wantCode string
	}{
		{name: "no entries", entries: nil, wantCallErr: "ValidationException"},
		{name: "eleven entries", entries: entries(11, nil), wantCallErr: "ValidationException"},
		{name: "ten entries", entries: entries(10, nil)},
		{name: "one entry", entries: entries(1, nil)},
		{name: "no Source", entries: entries(1, func(e *ebtypes.PutEventsRequestEntry) { e.Source = nil }), wantCode: "InvalidArgument"},
		{name: "no DetailType", entries: entries(1, func(e *ebtypes.PutEventsRequestEntry) { e.DetailType = aws.String("") }), wantCode: "InvalidArgument"},
		{name: "no Detail", entries: entries(1, func(e *ebtypes.PutEventsRequestEntry) { e.Detail = nil }), wantCode: "InvalidArgument"},
		{name: "a Detail that is not JSON", entries: entries(1, func(e *ebtypes.PutEventsRequestEntry) { e.Detail = aws.String("platform acme breached") }), wantCode: "MalformedDetail"},
		{name: "a Detail of JSON null", entries: entries(1, func(e *ebtypes.PutEventsRequestEntry) { e.Detail = aws.String("null") }), wantCode: "MalformedDetail"},
		{name: "a Detail that is a JSON array", entries: entries(1, func(e *ebtypes.PutEventsRequestEntry) { e.Detail = aws.String(`["acme"]`) }), wantCode: "MalformedDetail"},
		{
			name:    "an injected per-entry failure",
			entries: entries(1, nil),
			setup: func(f *awsfake.EventBridge) {
				f.EntryFailures = map[int]awsfake.EntryFailure{0: {Code: "ThrottlingException", Message: "rate exceeded"}}
			},
			wantCode: "ThrottlingException",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := awsfake.NewEventBridge()
			if c.setup != nil {
				c.setup(f)
			}
			out, err := f.PutEvents(context.Background(), &eventbridge.PutEventsInput{Entries: c.entries})
			if c.wantCallErr != "" {
				if apiCode(err) != c.wantCallErr {
					t.Errorf("got %v, want a whole-call %s", err, c.wantCallErr)
				}
				if len(f.Entries) != 0 {
					t.Errorf("a refused call published %d entries", len(f.Entries))
				}
				return
			}
			if err != nil {
				t.Fatalf("PutEvents: %v", err)
			}
			if len(out.Entries) != len(c.entries) {
				t.Fatalf("%d result entries for %d request entries", len(out.Entries), len(c.entries))
			}
			got := aws.ToString(out.Entries[0].ErrorCode)
			if got != c.wantCode {
				t.Errorf("first entry ErrorCode = %q, want %q", got, c.wantCode)
			}
			if c.wantCode == "" {
				if out.FailedEntryCount != 0 || len(f.Entries) != len(c.entries) || aws.ToString(out.Entries[0].EventId) == "" {
					t.Errorf("FailedEntryCount %d, published %d, EventId %q; want every entry published with an id",
						out.FailedEntryCount, len(f.Entries), aws.ToString(out.Entries[0].EventId))
				}
				return
			}
			if out.FailedEntryCount != 1 || len(f.Entries) != 0 || aws.ToString(out.Entries[0].ErrorMessage) == "" {
				t.Errorf("FailedEntryCount %d, published %d, ErrorMessage %q; want the entry failed, unpublished, with a message",
					out.FailedEntryCount, len(f.Entries), aws.ToString(out.Entries[0].ErrorMessage))
			}
		})
	}
}

// TestEventBridgePartialFailureKeepsEntryOrder: results line up with the
// entries they answer, the failed one counted and the rest published.
func TestEventBridgePartialFailureKeepsEntryOrder(t *testing.T) {
	f := awsfake.NewEventBridge()
	in := entries(3, nil)
	in[1].Detail = aws.String("{")
	out, err := f.PutEvents(context.Background(), &eventbridge.PutEventsInput{Entries: in})
	if err != nil {
		t.Fatal(err)
	}
	if out.FailedEntryCount != 1 || aws.ToString(out.Entries[1].ErrorCode) != "MalformedDetail" ||
		aws.ToString(out.Entries[0].EventId) == "" || aws.ToString(out.Entries[2].EventId) == "" {
		t.Errorf("results = %+v, want the middle entry failed and its neighbours published", out.Entries)
	}
	if len(f.Entries) != 2 || len(f.Calls) != 1 {
		t.Errorf("published %d entries over %d calls, want 2 over 1", len(f.Entries), len(f.Calls))
	}
}

// TestEventBridgeWholeCallError: Err fails the call with nothing published,
// and the call is still recorded.
func TestEventBridgeWholeCallError(t *testing.T) {
	boom := errors.New("boom")
	f := &awsfake.EventBridge{Err: boom}
	if _, err := f.PutEvents(context.Background(), &eventbridge.PutEventsInput{Entries: entries(1, nil)}); !errors.Is(err, boom) {
		t.Errorf("got %v, want the injected error", err)
	}
	if len(f.Calls) != 1 || len(f.Entries) != 0 {
		t.Errorf("Calls %d, Entries %d; want the attempt recorded and nothing published", len(f.Calls), len(f.Entries))
	}
}
