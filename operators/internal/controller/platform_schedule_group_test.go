/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// fakeScheduler records what the reconciler asked AWS for. `ensureArgoWorkflow`
// panicked on every call for its whole life because no test called it, so a
// function reached only through a reconcile path gets a caller here.
type fakeScheduler struct {
	existing  map[string]bool
	getErr    error
	createErr error

	gets    []string
	creates []string
	tags    map[string][]schedulertypes.Tag
}

func newFakeScheduler(existing ...string) *fakeScheduler {
	f := &fakeScheduler{existing: map[string]bool{}, tags: map[string][]schedulertypes.Tag{}}
	for _, e := range existing {
		f.existing[e] = true
	}
	return f
}

func (f *fakeScheduler) GetScheduleGroup(_ context.Context, in *scheduler.GetScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error) {
	f.gets = append(f.gets, aws.ToString(in.Name))
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.existing[aws.ToString(in.Name)] {
		return &scheduler.GetScheduleGroupOutput{Name: in.Name}, nil
	}
	return nil, &schedulertypes.ResourceNotFoundException{}
}

func (f *fakeScheduler) CreateScheduleGroup(_ context.Context, in *scheduler.CreateScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error) {
	name := aws.ToString(in.Name)
	f.creates = append(f.creates, name)
	f.tags[name] = in.Tags
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.existing[name] = true
	return &scheduler.CreateScheduleGroupOutput{}, nil
}

func schedulerPlatform(name string, caps ...platformv1alpha1.Capability) *platformv1alpha1.Platform {
	return platformWithCapabilities(name, caps...)
}

func TestEnsureScheduleGroup_CreatesTheTenantsGroupWhenAbsent(t *testing.T) {
	f := newFakeScheduler()
	r := &PlatformReconciler{Scheduler: f}

	p := schedulerPlatform("incident-response", platformv1alpha1.CapabilityEventBridgeScheduler)
	if err := r.ensureScheduleGroup(context.Background(), p, "development"); err != nil {
		t.Fatalf("ensureScheduleGroup: %v", err)
	}

	// The name the grant scopes to and the chart derives. If this diverges, every
	// CreateSchedule the tenant issues fails into its own log and nothing else
	// observes it — which is how the nudge path ran without ever firing.
	const want = "development-incident-response"
	if len(f.creates) != 1 || f.creates[0] != want {
		t.Fatalf("created groups: got %v, want exactly [%s]", f.creates, want)
	}
	if got := schedulerScheduleARN("development", p, testScope()); got != "arn:aws:scheduler:us-west-2:123456789012:schedule/"+want+"/*" {
		t.Errorf("the grant does not name the group that was created: %q", got)
	}

	byKey := map[string]string{}
	for _, tg := range f.tags[want] {
		byKey[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
	}
	if byKey["PlatformId"] != "incident-response" || byKey["Environment"] != "development" {
		t.Errorf("group tags do not attribute it to the tenant: %v", byKey)
	}
}

func TestEnsureScheduleGroup_IsIdempotent(t *testing.T) {
	f := newFakeScheduler("development-incident-response")
	r := &PlatformReconciler{Scheduler: f}

	p := schedulerPlatform("incident-response", platformv1alpha1.CapabilityEventBridgeScheduler)
	for i := 0; i < 3; i++ {
		if err := r.ensureScheduleGroup(context.Background(), p, "development"); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if len(f.creates) != 0 {
		t.Errorf("an existing group must not be recreated, got creates=%v", f.creates)
	}
}

func TestEnsureScheduleGroup_SkipsPlatformsWithoutTheCapability(t *testing.T) {
	f := newFakeScheduler()
	r := &PlatformReconciler{Scheduler: f}

	// A Platform declaring only SES, and one declaring nothing at all.
	for _, p := range []*platformv1alpha1.Platform{
		schedulerPlatform("ses-only", platformv1alpha1.CapabilitySES),
		schedulerPlatform("bare"),
	} {
		if err := r.ensureScheduleGroup(context.Background(), p, "development"); err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
	}
	if len(f.gets) != 0 || len(f.creates) != 0 {
		t.Errorf("a Platform without the capability must make no Scheduler call, got gets=%v creates=%v", f.gets, f.creates)
	}
}

func TestEnsureScheduleGroup_NoClientIsNotAnError(t *testing.T) {
	r := &PlatformReconciler{}
	p := schedulerPlatform("incident-response", platformv1alpha1.CapabilityEventBridgeScheduler)
	if err := r.ensureScheduleGroup(context.Background(), p, "development"); err != nil {
		t.Errorf("an unwired client must no-op, matching every other client on the reconciler: %v", err)
	}
}

// A concurrent reconcile of the same Platform can lose the create race. The
// postcondition is that the group exists, so the conflict is success — not an
// error that would fail the whole IAM reconcile and leave the tenant role
// half-written.
func TestEnsureScheduleGroup_ConflictIsSuccess(t *testing.T) {
	f := newFakeScheduler()
	f.createErr = &schedulertypes.ConflictException{}
	r := &PlatformReconciler{Scheduler: f}

	p := schedulerPlatform("incident-response", platformv1alpha1.CapabilityEventBridgeScheduler)
	if err := r.ensureScheduleGroup(context.Background(), p, "development"); err != nil {
		t.Errorf("a create conflict means the group exists, which is the postcondition: %v", err)
	}
}

// Any other failure must surface. A group that silently does not exist is the
// dead path this whole change closes, so swallowing the error would reproduce it
// one layer up.
func TestEnsureScheduleGroup_RealFailuresSurface(t *testing.T) {
	t.Run("get", func(t *testing.T) {
		f := newFakeScheduler()
		f.getErr = errors.New("AccessDeniedException: not authorized to GetScheduleGroup")
		r := &PlatformReconciler{Scheduler: f}
		if err := r.ensureScheduleGroup(context.Background(), schedulerPlatform("ir", platformv1alpha1.CapabilityEventBridgeScheduler), "development"); err == nil {
			t.Error("a denied GetScheduleGroup must fail the reconcile, not be read as absent")
		}
	})
	t.Run("create", func(t *testing.T) {
		f := newFakeScheduler()
		f.createErr = errors.New("AccessDeniedException: not authorized to CreateScheduleGroup")
		r := &PlatformReconciler{Scheduler: f}
		if err := r.ensureScheduleGroup(context.Background(), schedulerPlatform("ir", platformv1alpha1.CapabilityEventBridgeScheduler), "development"); err == nil {
			t.Error("a denied CreateScheduleGroup must fail the reconcile")
		}
	})
}
