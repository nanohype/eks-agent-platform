/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/scheduler"
)

// Scheduler is the slice of aws-sdk-go-v2/scheduler the Platform reconciler uses
// to give a tenant declaring the eventBridgeScheduler capability a schedule group
// of its own.
//
// The group is the isolation boundary. An EventBridge Scheduler schedule ARN is
// `schedule/<group>/<name>`, so scoping a grant to one group is an exact match on
// the path segment — where a name-prefix grant inside the shared `default` group
// spans any tenant whose name is a hyphen-prefix of another's, and Platform names
// are DNS-1123 labels with nothing forbidding that pair.
//
// Create and read only. Deleting a schedule group deletes every schedule in it,
// and those are written by the tenant at runtime, not by this operator — the same
// reason the operator holds no delete on any datastore. A group outlives its
// Platform.
type Scheduler interface {
	GetScheduleGroup(ctx context.Context, params *scheduler.GetScheduleGroupInput, optFns ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error)
	CreateScheduleGroup(ctx context.Context, params *scheduler.CreateScheduleGroupInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error)
}

var _ Scheduler = (*scheduler.Client)(nil)
