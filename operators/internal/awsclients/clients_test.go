/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsclients

import (
	"context"
	"testing"
	"time"

	smithymiddleware "github.com/aws/smithy-go/middleware"
)

// TestWithOperationTimeout verifies the middleware registers on the Initialize
// step and decorates the operation context with a deadline within the budget,
// so every AWS operation is bounded regardless of the SDK's retry behaviour.
func TestWithOperationTimeout(t *testing.T) {
	stack := smithymiddleware.NewStack("test", func() interface{} { return nil })
	if err := withOperationTimeout(50 * time.Millisecond)(stack); err != nil {
		t.Fatalf("apply middleware: %v", err)
	}

	m, ok := stack.Initialize.Get("OperationTimeout")
	if !ok {
		t.Fatal("OperationTimeout not registered on the Initialize step")
	}

	var deadline time.Time
	var sawDeadline bool
	next := smithymiddleware.InitializeHandlerFunc(func(ctx context.Context, _ smithymiddleware.InitializeInput) (smithymiddleware.InitializeOutput, smithymiddleware.Metadata, error) {
		deadline, sawDeadline = ctx.Deadline()
		return smithymiddleware.InitializeOutput{}, smithymiddleware.Metadata{}, nil
	})

	if _, _, err := m.HandleInitialize(context.Background(), smithymiddleware.InitializeInput{}, next); err != nil {
		t.Fatalf("handle initialize: %v", err)
	}

	if !sawDeadline {
		t.Fatal("operation context carried no deadline")
	}
	if budget := time.Until(deadline); budget <= 0 || budget > 50*time.Millisecond {
		t.Fatalf("deadline budget out of range: %v", budget)
	}
}
