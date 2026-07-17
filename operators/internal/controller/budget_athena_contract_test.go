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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// These are contract tests pinning the exact Athena and CloudWatch response
// shapes the Budget reconciler's spend math depends on. The fakes return
// recorded, representative responses — the QueryExecution status transitions and
// the CUR-rollup ResultSet Athena actually returns, and the per-minute
// EstimatedInvocationCostUsd datapoints CloudWatch returns — so an upstream field
// rename (Rows[].Data[].VarCharValue, QueryExecution.Status.State,
// MetricDataResults[].Values) surfaces here as a test failure rather than as a
// silently-wrong spend number in production.

// fakeAthena drives the query lifecycle: StartQueryExecution → a scripted
// sequence of GetQueryExecution states → GetQueryResults. It records the query
// string so the CUR-rollup contract can be asserted.
type fakeAthena struct {
	states    []athenatypes.QueryExecutionState // returned in order across GetQueryExecution calls
	stateIdx  int
	results   *athena.GetQueryResultsOutput
	failure   string
	startErr  error
	getErr    error
	resultErr error

	lastQuery string
	stopped   bool
}

func (f *fakeAthena) StartQueryExecution(_ context.Context, in *athena.StartQueryExecutionInput, _ ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.lastQuery = aws.ToString(in.QueryString)
	return &athena.StartQueryExecutionOutput{QueryExecutionId: aws.String("qid-1")}, nil
}

func (f *fakeAthena) GetQueryExecution(_ context.Context, _ *athena.GetQueryExecutionInput, _ ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	state := f.states[f.stateIdx]
	if f.stateIdx < len(f.states)-1 {
		f.stateIdx++
	}
	return &athena.GetQueryExecutionOutput{QueryExecution: &athenatypes.QueryExecution{
		Status: &athenatypes.QueryExecutionStatus{State: state, StateChangeReason: aws.String(f.failure)},
	}}, nil
}

func (f *fakeAthena) GetQueryResults(_ context.Context, _ *athena.GetQueryResultsInput, _ ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	if f.resultErr != nil {
		return nil, f.resultErr
	}
	return f.results, nil
}

func (f *fakeAthena) StopQueryExecution(_ context.Context, _ *athena.StopQueryExecutionInput, _ ...func(*athena.Options)) (*athena.StopQueryExecutionOutput, error) {
	f.stopped = true
	return &athena.StopQueryExecutionOutput{}, nil
}

// curRollupResultSet is the recorded shape of a CUR month-to-date rollup:
// a header row (the SELECT alias) followed by one data row carrying the summed
// unblended cost as a VarCharValue string (Athena returns all values as strings).
func curRollupResultSet(spend string) *athena.GetQueryResultsOutput {
	return &athena.GetQueryResultsOutput{ResultSet: &athenatypes.ResultSet{Rows: []athenatypes.Row{
		{Data: []athenatypes.Datum{{VarCharValue: aws.String("spend_usd")}}},
		{Data: []athenatypes.Datum{{VarCharValue: aws.String(spend)}}},
	}}}
}

func succeededAthena(results *athena.GetQueryResultsOutput) *fakeAthena {
	return &fakeAthena{states: []athenatypes.QueryExecutionState{athenatypes.QueryExecutionStateSucceeded}, results: results}
}

func budgetReconciler(a *fakeAthena) *BudgetReconciler {
	return &BudgetReconciler{
		Athena:          a,
		RequeueInterval: time.Minute,
		AthenaCfg:       AthenaConfig{Workgroup: "cost_wg", Database: "cost_db", CURTableName: "cur_eks_agent_platform"},
	}
}

func TestQuerySpendFromAthena_ContractHappyPath(t *testing.T) {
	a := succeededAthena(curRollupResultSet("1512.734500"))
	spend, err := budgetReconciler(a).querySpendFromAthena(context.Background(), "acme")
	if err != nil {
		t.Fatalf("querySpendFromAthena: %v", err)
	}
	if spend != "1512.734500" {
		t.Errorf("spend: got %q want 1512.734500", spend)
	}
	// The CUR-rollup query contract: it filters on the PlatformId user-tag column
	// and sums the unblended cost month-to-date.
	for _, want := range []string{"resource_tags_user_platformid = 'acme'", "line_item_unblended_cost", "date_trunc('month'"} {
		if !strings.Contains(a.lastQuery, want) {
			t.Errorf("CUR rollup query missing %q:\n%s", want, a.lastQuery)
		}
	}
}

func TestFetchAthenaResultDecimal_ContractShapes(t *testing.T) {
	r := budgetReconciler(nil)
	cases := map[string]struct {
		out  *athena.GetQueryResultsOutput
		want string
	}{
		"data row present": {curRollupResultSet("42.500000"), "42.500000"},
		"header only (no MTD spend)": {
			&athena.GetQueryResultsOutput{ResultSet: &athenatypes.ResultSet{Rows: []athenatypes.Row{
				{Data: []athenatypes.Datum{{VarCharValue: aws.String("spend_usd")}}},
			}}}, "0",
		},
		"null cell": {
			&athena.GetQueryResultsOutput{ResultSet: &athenatypes.ResultSet{Rows: []athenatypes.Row{
				{Data: []athenatypes.Datum{{VarCharValue: aws.String("spend_usd")}}},
				{Data: []athenatypes.Datum{{VarCharValue: nil}}},
			}}}, "0",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r.Athena = &fakeAthena{results: c.out}
			got, err := r.fetchAthenaResultDecimal(context.Background(), "qid")
			if err != nil {
				t.Fatalf("fetchAthenaResultDecimal: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestQuerySpendFromAthena_ErrorPaths(t *testing.T) {
	t.Run("query failed terminal", func(t *testing.T) {
		a := &fakeAthena{states: []athenatypes.QueryExecutionState{athenatypes.QueryExecutionStateFailed}, failure: "SYNTAX_ERROR"}
		if _, err := budgetReconciler(a).querySpendFromAthena(context.Background(), "acme"); err == nil {
			t.Fatal("a FAILED query must surface as an error")
		}
	})
	t.Run("start error", func(t *testing.T) {
		a := &fakeAthena{startErr: errors.New("denied")}
		if _, err := budgetReconciler(a).querySpendFromAthena(context.Background(), "acme"); err == nil {
			t.Fatal("a StartQueryExecution error must propagate")
		}
	})
	t.Run("get-execution error", func(t *testing.T) {
		a := &fakeAthena{states: []athenatypes.QueryExecutionState{athenatypes.QueryExecutionStateRunning}, getErr: errors.New("boom")}
		if _, err := budgetReconciler(a).querySpendFromAthena(context.Background(), "acme"); err == nil {
			t.Fatal("a GetQueryExecution error must propagate")
		}
	})
	t.Run("not configured", func(t *testing.T) {
		r := &BudgetReconciler{Athena: succeededAthena(curRollupResultSet("0"))} // no AthenaCfg
		if _, err := r.querySpendFromAthena(context.Background(), "acme"); err == nil {
			t.Fatal("an unconfigured workgroup/database must error")
		}
	})
	t.Run("rejects an invalid identifier", func(t *testing.T) {
		r := budgetReconciler(succeededAthena(curRollupResultSet("0")))
		r.AthenaCfg.Database = "bad;drop"
		if _, err := r.querySpendFromAthena(context.Background(), "acme"); err == nil {
			t.Fatal("an identifier failing validation must refuse to build the query")
		}
	})
	t.Run("nil client", func(t *testing.T) {
		if _, err := (&BudgetReconciler{}).querySpendFromAthena(context.Background(), "acme"); err == nil {
			t.Fatal("a nil Athena client must error (not configured)")
		}
	})
}

// fakeCloudWatch returns a recorded GetMetricData response: per-minute Sum
// datapoints of EstimatedInvocationCostUsd, the shape queryInflightCost folds
// into a single decimal.
type fakeCloudWatch struct {
	values []float64
	err    error
	empty  bool
}

func (f *fakeCloudWatch) GetMetricData(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return &cloudwatch.GetMetricDataOutput{}, nil
	}
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: []cloudwatchtypes.MetricDataResult{{
		Id:     aws.String("inflight"),
		Values: f.values,
	}}}, nil
}

func TestQueryInflightCost_ContractAndBranches(t *testing.T) {
	since := time.Now().Add(-time.Hour)
	t.Run("sums the per-minute datapoints", func(t *testing.T) {
		r := &BudgetReconciler{CloudWatch: &fakeCloudWatch{values: []float64{0.25, 0.5, 1.25}}}
		got, err := r.queryInflightCost(context.Background(), "acme", since)
		if err != nil {
			t.Fatalf("queryInflightCost: %v", err)
		}
		if got != "2.000000" {
			t.Errorf("summed in-flight cost: got %q want 2.000000", got)
		}
	})
	t.Run("no datapoints yields zero", func(t *testing.T) {
		r := &BudgetReconciler{CloudWatch: &fakeCloudWatch{empty: true}}
		if got, _ := r.queryInflightCost(context.Background(), "acme", since); got != "0" {
			t.Errorf("empty results must yield 0, got %q", got)
		}
	})
	t.Run("nil client yields zero", func(t *testing.T) {
		if got, _ := (&BudgetReconciler{}).queryInflightCost(context.Background(), "acme", since); got != "0" {
			t.Errorf("nil CloudWatch must yield 0, got %q", got)
		}
	})
	t.Run("future since yields zero", func(t *testing.T) {
		r := &BudgetReconciler{CloudWatch: &fakeCloudWatch{values: []float64{1}}}
		if got, _ := r.queryInflightCost(context.Background(), "acme", time.Now().Add(time.Hour)); got != "0" {
			t.Errorf("a since after now must yield 0, got %q", got)
		}
	})
	t.Run("error yields zero and the error", func(t *testing.T) {
		r := &BudgetReconciler{CloudWatch: &fakeCloudWatch{err: errors.New("throttled")}}
		if _, err := r.queryInflightCost(context.Background(), "acme", since); err == nil {
			t.Fatal("a GetMetricData error must propagate")
		}
	})
}

func TestQueryTimeout(t *testing.T) {
	if got := (&BudgetReconciler{}).queryTimeout(); got != 2*time.Minute {
		t.Errorf("default queryTimeout: got %v want 2m", got)
	}
	if got := (&BudgetReconciler{RequeueInterval: 10 * time.Minute}).queryTimeout(); got != 5*time.Minute {
		t.Errorf("half-interval queryTimeout: got %v want 5m", got)
	}
	if got := (&BudgetReconciler{RequeueInterval: 20 * time.Second}).queryTimeout(); got != 30*time.Second {
		t.Errorf("floored queryTimeout: got %v want 30s", got)
	}
}

func TestSleepCtx_RespectsCancellation(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepCtx should complete: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Fatal("sleepCtx must return the context error when canceled")
	}
}
