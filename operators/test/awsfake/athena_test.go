/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package awsfake_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/nanohype/eks-agent-platform/operators/test/awsfake"
)

// TestAthenaHonoursTheClientRequestToken: a StartQueryExecution repeating an
// earlier token with the same parameters starts nothing and returns the first
// execution's id; one repeating it with another query or another workgroup is
// refused; a call with no token always starts a new execution.
func TestAthenaHonoursTheClientRequestToken(t *testing.T) {
	const token = "00000000-0000-4000-8000-000000000001"
	start := func(f *awsfake.Athena, query string, token *string) (string, error) {
		out, err := f.StartQueryExecution(context.Background(), &athena.StartQueryExecutionInput{QueryString: aws.String(query), ClientRequestToken: token})
		if err != nil {
			return "", err
		}
		return aws.ToString(out.QueryExecutionId), nil
	}
	cases := []struct {
		name string
		// second is the second call, after a first call of "SELECT 1" with token.
		second      func(*awsfake.Athena) (string, error)
		wantSame    bool
		wantRefused bool
		wantStarted int
	}{
		{
			name:        "the same token and query",
			second:      func(f *awsfake.Athena) (string, error) { return start(f, "SELECT 1", aws.String(token)) },
			wantSame:    true,
			wantStarted: 1,
		},
		{
			name:        "the same token with another query",
			second:      func(f *awsfake.Athena) (string, error) { return start(f, "SELECT 2", aws.String(token)) },
			wantRefused: true,
			wantStarted: 1,
		},
		{
			name: "the same token and query in another workgroup",
			second: func(f *awsfake.Athena) (string, error) {
				out, err := f.StartQueryExecution(context.Background(), &athena.StartQueryExecutionInput{
					QueryString: aws.String("SELECT 1"), WorkGroup: aws.String("other_wg"), ClientRequestToken: aws.String(token),
				})
				if err != nil {
					return "", err
				}
				return aws.ToString(out.QueryExecutionId), nil
			},
			wantRefused: true,
			wantStarted: 1,
		},
		{
			name:        "another token",
			second:      func(f *awsfake.Athena) (string, error) { return start(f, "SELECT 1", aws.String(token+"-2")) },
			wantStarted: 2,
		},
		{
			name:        "no token",
			second:      func(f *awsfake.Athena) (string, error) { return start(f, "SELECT 1", nil) },
			wantStarted: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := awsfake.NewAthena(athenatypes.QueryExecutionStateSucceeded)
			first, err := start(f, "SELECT 1", aws.String(token))
			if err != nil {
				t.Fatal(err)
			}
			second, err := c.second(f)
			switch {
			case c.wantRefused:
				if !is[*athenatypes.InvalidRequestException](err) {
					t.Errorf("got (%q, %v), want InvalidRequestException", second, err)
				}
			case err != nil:
				t.Fatalf("second start: %v", err)
			case c.wantSame && second != first:
				t.Errorf("a repeated token returned %q, want the first execution's id %q", second, first)
			case !c.wantSame && second == first:
				t.Errorf("a new execution reused the id %q", first)
			}
			if len(f.Started) != c.wantStarted || len(f.StartCalls) != 2 {
				t.Errorf("started %v from %d calls, want %d executions from 2", f.Started, len(f.StartCalls), c.wantStarted)
			}
		})
	}
}

// TestAthenaScriptsTheLifecycle: states run in order and the last repeats, a
// fake with none scripted says so rather than panicking, and a stop is
// recorded.
func TestAthenaScriptsTheLifecycle(t *testing.T) {
	ctx := context.Background()
	f := awsfake.NewAthena(athenatypes.QueryExecutionStateQueued, athenatypes.QueryExecutionStateRunning, athenatypes.QueryExecutionStateSucceeded)
	var got []athenatypes.QueryExecutionState
	for i := 0; i < 4; i++ {
		out, err := f.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{QueryExecutionId: aws.String("qid-1")})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out.QueryExecution.Status.State)
	}
	want := []athenatypes.QueryExecutionState{"QUEUED", "RUNNING", "SUCCEEDED", "SUCCEEDED"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("states = %v, want %v", got, want)
		}
	}
	if _, err := awsfake.NewAthena().GetQueryExecution(ctx, &athena.GetQueryExecutionInput{QueryExecutionId: aws.String("qid-1")}); err == nil {
		t.Error("GetQueryExecution with no States scripted answered")
	}
	if out, err := f.GetQueryResults(ctx, &athena.GetQueryResultsInput{QueryExecutionId: aws.String("qid-1")}); err != nil || out.ResultSet == nil {
		t.Errorf("GetQueryResults with no Results scripted = (%+v, %v), want an empty result set", out, err)
	}
	if _, err := f.StopQueryExecution(ctx, &athena.StopQueryExecutionInput{QueryExecutionId: aws.String("qid-1")}); err != nil || !f.Stopped || len(f.StopCalls) != 1 {
		t.Errorf("StopQueryExecution: err %v, Stopped %v, StopCalls %d", err, f.Stopped, len(f.StopCalls))
	}
}
