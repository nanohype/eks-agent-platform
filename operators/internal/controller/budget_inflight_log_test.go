/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// recordedError captures one logr.Error call for assertion.
type recordedError struct {
	err error
	msg string
}

// recordingSink is a logr.LogSink that captures Error calls, letting a test
// assert the reconciler actually logged a swallowed failure rather than
// silently discarding it. Info calls are dropped.
type recordingSink struct {
	errs *[]recordedError
}

func (s recordingSink) Init(logr.RuntimeInfo)    {}
func (s recordingSink) Enabled(int) bool         { return true }
func (s recordingSink) Info(int, string, ...any) {}
func (s recordingSink) Error(err error, msg string, _ ...any) {
	*s.errs = append(*s.errs, recordedError{err: err, msg: msg})
}
func (s recordingSink) WithValues(...any) logr.LogSink { return s }
func (s recordingSink) WithName(string) logr.LogSink   { return s }

// TestReconcileBudget_InflightFailureIsLogged is a regression guard for the
// CloudWatch-outage branch in reconcileBudget: when the in-flight cost query
// fails the reconciler zeros the in-flight portion, and it MUST log that it did
// so. A silent zero would undercount spend against the budget on every tick with
// no operator-visible signal — the code comment claims "we log and zero out", so
// this pins the log to the behavior.
func TestReconcileBudget_InflightFailureIsLogged(t *testing.T) {
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "cw-acme", Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseReady},
	}
	cl := fake.NewClientBuilder().WithScheme(killSwitchTestScheme(t)).WithObjects(platform).Build()

	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cw-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "cw-acme"}, MonthlyUsd: "100.00",
		},
	}

	// Athena unset (dev path → CUR spend 0); CloudWatch errors so the in-flight
	// branch fires.
	cwErr := errors.New("cloudwatch throttled")
	r := &BudgetReconciler{Client: cl, CloudWatch: &fakeCloudWatch{err: cwErr}, RequeueInterval: time.Hour}

	var captured []recordedError
	ctx := log.IntoContext(context.Background(), logr.New(recordingSink{errs: &captured}))

	reading, err := r.reconcileBudget(ctx, bp)
	if err != nil {
		t.Fatalf("reconcileBudget: %v", err)
	}
	// The in-flight portion zeroed out; with no CUR spend, total is 0.
	if reading.spendUsd != "0.000000" {
		t.Errorf("spendUsd = %q, want the in-flight portion zeroed (0.000000)", reading.spendUsd)
	}

	var found *recordedError
	for i := range captured {
		if errors.Is(captured[i].err, cwErr) {
			found = &captured[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected an Error log carrying the CloudWatch failure; captured %d error log(s): %+v", len(captured), captured)
	}
	if !strings.Contains(found.msg, "in-flight") {
		t.Errorf("log message %q should describe the zeroed in-flight portion", found.msg)
	}
}
