/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// scheduleGroupName is the EventBridge Scheduler group a Platform declaring the
// eventBridgeScheduler capability owns. Composed the same way every other
// tenant-scoped AWS name in this system is — <env>-<platform> — so the operator's
// grant, the invoke role's trust condition and the tenant chart all derive it
// rather than being handed it. Nothing publishes it, because there is nothing to
// publish that the composer does not already know.
func scheduleGroupName(env string, p *platformv1alpha1.Platform) string {
	return fmt.Sprintf("%s-%s", env, p.Name)
}

// ensureScheduleGroup makes the tenant's schedule group exist.
//
// Without it every CreateSchedule the tenant issues fails — with
// ResourceNotFoundException if the group is missing, and with AccessDenied if it
// exists but the grant names a different one. Both surface only in the caller's
// own error handling, which is how the incident-response nudge path ran for its
// whole life without firing a single schedule: the app logged a warning and
// carried on, and no gate anywhere reads a runtime log.
//
// Create-if-absent, never delete. Deleting a schedule group deletes every
// schedule inside it, and those are the tenant's, written at runtime. The
// operator holds no delete here for the same reason it holds none on a
// datastore: the isolation boundary is enforced by permission, not by finalizer
// logic. A group outlives its Platform, which costs nothing — an empty schedule
// group is not billed.
func (r *PlatformReconciler) ensureScheduleGroup(ctx context.Context, p *platformv1alpha1.Platform, env string) error {
	if !hasCapability(p, platformv1alpha1.CapabilityEventBridgeScheduler) {
		return nil
	}
	if r.Scheduler == nil {
		return nil
	}

	name := scheduleGroupName(env, p)

	_, err := r.Scheduler.GetScheduleGroup(ctx, &scheduler.GetScheduleGroupInput{Name: aws.String(name)})
	if err == nil {
		return nil
	}
	var notFound *schedulertypes.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		return fmt.Errorf("get schedule group %s: %w", name, err)
	}

	_, err = r.Scheduler.CreateScheduleGroup(ctx, &scheduler.CreateScheduleGroupInput{
		Name: aws.String(name),
		Tags: []schedulertypes.Tag{
			{Key: aws.String("PlatformId"), Value: aws.String(p.Name)},
			{Key: aws.String("Environment"), Value: aws.String(env)},
			{Key: aws.String("ManagedBy"), Value: aws.String("eks-agent-platform")},
		},
	})
	if err != nil {
		// A concurrent reconcile of the same Platform can win the race. The
		// group existing is the whole postcondition, so that is success.
		var conflict *schedulertypes.ConflictException
		if errors.As(err, &conflict) {
			return nil
		}
		return fmt.Errorf("create schedule group %s: %w", name, err)
	}
	return nil
}
