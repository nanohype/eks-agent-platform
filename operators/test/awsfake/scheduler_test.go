/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"

	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

// TestSchedulerRefusesWhatSchedulerRefuses: reading a group that does not
// exist is ResourceNotFound, and creating one that does is a conflict.
func TestSchedulerRefusesWhatSchedulerRefuses(t *testing.T) {
	ctx := context.Background()
	get := func(f *awsfake.Scheduler, name string) error {
		_, err := f.GetScheduleGroup(ctx, &scheduler.GetScheduleGroupInput{Name: aws.String(name)})
		return err
	}
	create := func(f *awsfake.Scheduler, name string) error {
		_, err := f.CreateScheduleGroup(ctx, &scheduler.CreateScheduleGroupInput{Name: aws.String(name)})
		return err
	}
	cases := []struct {
		name string
		call func(*awsfake.Scheduler) error
		want func(error) bool
	}{
		{name: "reading an absent group", call: func(f *awsfake.Scheduler) error { return get(f, "dev-b") },
			want: func(err error) bool { return is[*schedulertypes.ResourceNotFoundException](err) }},
		{name: "reading a held group", call: func(f *awsfake.Scheduler) error { return get(f, "dev-a") }},
		{name: "creating a held group", call: func(f *awsfake.Scheduler) error { return create(f, "dev-a") },
			want: func(err error) bool { return is[*schedulertypes.ConflictException](err) }},
		{name: "creating an absent group", call: func(f *awsfake.Scheduler) error {
			if err := create(f, "dev-b"); err != nil {
				return err
			}
			return get(f, "dev-b")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(awsfake.NewScheduler("dev-a"))
			switch {
			case c.want == nil && err != nil:
				t.Errorf("got %v, want success", err)
			case c.want != nil && !c.want(err):
				t.Errorf("got %v, want the refusal Scheduler answers with", err)
			}
		})
	}
}
