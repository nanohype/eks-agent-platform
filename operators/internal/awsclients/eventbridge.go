/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
)

// EventBridge is the slice of aws-sdk-go-v2/eventbridge the Budget
// reconciler uses to publish kill-switch events to the bus created by
// terraform/components/kill-switch. The bus has a single rule that
// targets the Step Functions state machine which suspends offending
// Platforms (revokes IRSA, scales fleets to zero).
type EventBridge interface {
	PutEvents(ctx context.Context, params *eventbridge.PutEventsInput, optFns ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error)
}

var _ EventBridge = (*eventbridge.Client)(nil)
