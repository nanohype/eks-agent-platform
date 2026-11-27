/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/smithy-go"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// MaxPutEventsEntries is the most entries one PutEvents call may carry.
const MaxPutEventsEntries = 10

var _ awsclients.EventBridge = (*EventBridge)(nil)

// EventBridge is an in-memory awsclients.EventBridge that publishes PutEvents
// entries and records what it published.
//
// It refuses what EventBridge refuses. A call carrying no entry or more than
// MaxPutEventsEntries fails whole with ValidationException. Within a call, each
// entry with no Source, DetailType or Detail fails with InvalidArgument, and
// each whose Detail is not a JSON object with MalformedDetail; those count in
// FailedEntryCount and the call itself succeeds, as PutEvents answers 200 on a
// partial failure. Results come back in entry order, an EventId for each
// published entry and an ErrorCode and ErrorMessage for each failed one.
type EventBridge struct {
	sync.Mutex

	// Calls records every PutEvents call, ahead of Err and every check.
	Calls []eventbridge.PutEventsInput
	// Entries records every entry published, in order, across calls.
	Entries []ebtypes.PutEventsRequestEntry

	// Err fails every PutEvents call whole.
	Err error
	// EntryFailures fails the entry at each index of a call, counted from 0
	// within that call, with the given code and message. It applies to an
	// entry that passes EventBridge's own checks, and to every call.
	EntryFailures map[int]EntryFailure
}

// EntryFailure is a per-entry PutEvents failure: the result entry's ErrorCode
// and ErrorMessage.
type EntryFailure struct {
	Code    string
	Message string
}

// NewEventBridge returns an EventBridge that publishes every valid entry.
func NewEventBridge() *EventBridge {
	return &EventBridge{}
}

// PutEvents publishes each valid entry of the call.
func (f *EventBridge) PutEvents(_ context.Context, in *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.Calls = append(f.Calls, *in)
	if f.Err != nil {
		return nil, f.Err
	}
	if n := len(in.Entries); n < 1 || n > MaxPutEventsEntries {
		return nil, &smithy.GenericAPIError{
			Code: "ValidationException",
			Message: fmt.Sprintf("1 validation error detected: Value at 'entries' failed to satisfy constraint: "+
				"Member must have length between 1 and %d (got %d)", MaxPutEventsEntries, n),
		}
	}
	out := &eventbridge.PutEventsOutput{Entries: make([]ebtypes.PutEventsResultEntry, 0, len(in.Entries))}
	for i, entry := range in.Entries {
		if failure, failed := f.entryFailure(i, entry); failed {
			out.FailedEntryCount++
			out.Entries = append(out.Entries, ebtypes.PutEventsResultEntry{
				ErrorCode:    aws.String(failure.Code),
				ErrorMessage: aws.String(failure.Message),
			})
			continue
		}
		f.Entries = append(f.Entries, entry)
		out.Entries = append(out.Entries, ebtypes.PutEventsResultEntry{EventId: aws.String(fmt.Sprintf("event-%d", len(f.Entries)))})
	}
	return out, nil
}

// entryFailure is why the entry at index i of a call fails, if it does.
// Callers hold f's lock.
func (f *EventBridge) entryFailure(i int, entry ebtypes.PutEventsRequestEntry) (EntryFailure, bool) {
	for _, required := range []struct {
		field string
		value *string
	}{{"Source", entry.Source}, {"DetailType", entry.DetailType}, {"Detail", entry.Detail}} {
		if aws.ToString(required.value) == "" {
			return EntryFailure{
				Code:    "InvalidArgument",
				Message: "Parameter " + required.field + " is not valid. Reason: " + required.field + " is a required argument.",
			}, true
		}
	}
	if !isJSONObject(aws.ToString(entry.Detail)) {
		return EntryFailure{Code: "MalformedDetail", Message: "Detail is malformed."}, true
	}
	failure, ok := f.EntryFailures[i]
	return failure, ok
}
