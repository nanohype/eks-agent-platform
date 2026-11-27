/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"

	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

var _ awsclients.Scheduler = (*Scheduler)(nil)

// Scheduler is an in-memory awsclients.Scheduler holding schedule groups by
// name and recording what it was asked for. GetScheduleGroup on a group it
// does not hold gets ResourceNotFoundException, and CreateScheduleGroup of one
// it already holds gets ConflictException, as EventBridge Scheduler answers.
type Scheduler struct {
	sync.Mutex

	// Existing holds the schedule groups that exist.
	Existing map[string]bool
	// GetErr and CreateErr fail GetScheduleGroup and CreateScheduleGroup.
	GetErr    error
	CreateErr error

	// Gets records the group every GetScheduleGroup call named, ahead of
	// GetErr.
	Gets []string
	// Creates records the group every CreateScheduleGroup call named, and Tags
	// the tags it carried, ahead of CreateErr.
	Creates []string
	Tags    map[string][]schedulertypes.Tag
}

// NewScheduler returns a Scheduler holding the existing groups.
func NewScheduler(existing ...string) *Scheduler {
	f := &Scheduler{Existing: map[string]bool{}, Tags: map[string][]schedulertypes.Tag{}}
	for _, e := range existing {
		f.Existing[e] = true
	}
	return f
}

// GetScheduleGroup returns a group that exists.
func (f *Scheduler) GetScheduleGroup(_ context.Context, in *scheduler.GetScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error) {
	f.Lock()
	defer f.Unlock()
	f.Gets = append(f.Gets, aws.ToString(in.Name))
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	if f.Existing[aws.ToString(in.Name)] {
		return &scheduler.GetScheduleGroupOutput{Name: in.Name, State: schedulertypes.ScheduleGroupStateActive}, nil
	}
	return nil, &schedulertypes.ResourceNotFoundException{Message: aws.String("Schedule group " + aws.ToString(in.Name) + " does not exist.")}
}

// CreateScheduleGroup creates a group that does not exist.
func (f *Scheduler) CreateScheduleGroup(_ context.Context, in *scheduler.CreateScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error) {
	f.Lock()
	defer f.Unlock()
	name := aws.ToString(in.Name)
	f.Creates = append(f.Creates, name)
	if f.Tags == nil {
		f.Tags = map[string][]schedulertypes.Tag{}
	}
	f.Tags[name] = in.Tags
	if f.CreateErr != nil {
		return nil, f.CreateErr
	}
	if f.Existing[name] {
		return nil, &schedulertypes.ConflictException{Message: aws.String("Schedule group " + name + " already exists.")}
	}
	if f.Existing == nil {
		f.Existing = map[string]bool{}
	}
	f.Existing[name] = true
	return &scheduler.CreateScheduleGroupOutput{}, nil
}
