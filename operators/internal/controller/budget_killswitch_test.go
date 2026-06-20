/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

// fakeEventBridge is a minimal in-memory awsclients.EventBridge that returns a
// canned PutEvents result so the partial-failure branch can be asserted.
type fakeEventBridge struct {
	out   *eventbridge.PutEventsOutput
	calls []eventbridge.PutEventsInput
}

func (f *fakeEventBridge) PutEvents(_ context.Context, params *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	f.calls = append(f.calls, *params)
	return f.out, nil
}

func newBudgetPolicy() *governancev1alpha1.BudgetPolicy {
	return &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "acme"},
			MonthlyUsd:  "100.00",
		},
	}
}

func TestFireKillSwitch_SuccessReturnsNil(t *testing.T) {
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{FailedEntryCount: 0}}
	r := &BudgetReconciler{EventBridge: eb, KillSwitchEventBusName: "killswitch-bus"}

	if err := r.fireKillSwitch(context.Background(), newBudgetPolicy(), "150.00", 150); err != nil {
		t.Fatalf("fireKillSwitch on a clean PutEvents must return nil, got %v", err)
	}
	if len(eb.calls) != 1 {
		t.Fatalf("want exactly 1 PutEvents call, got %d", len(eb.calls))
	}
	if got := aws.ToString(eb.calls[0].Entries[0].EventBusName); got != "killswitch-bus" {
		t.Errorf("event bus = %q, want killswitch-bus", got)
	}
}

func TestFireKillSwitch_PartialFailureIsRetryableError(t *testing.T) {
	eb := &fakeEventBridge{out: &eventbridge.PutEventsOutput{
		FailedEntryCount: 1,
		Entries: []ebtypes.PutEventsResultEntry{{
			ErrorCode:    aws.String("ThrottlingException"),
			ErrorMessage: aws.String("rate exceeded"),
		}},
	}}
	r := &BudgetReconciler{EventBridge: eb, KillSwitchEventBusName: "killswitch-bus"}

	err := r.fireKillSwitch(context.Background(), newBudgetPolicy(), "150.00", 150)
	if err == nil {
		t.Fatal("FailedEntryCount>0 must surface as an error so the kill-switch is retried, got nil")
	}
	if !strings.Contains(err.Error(), "ThrottlingException") {
		t.Errorf("error must carry the failed entry's ErrorCode, got %q", err.Error())
	}
}
