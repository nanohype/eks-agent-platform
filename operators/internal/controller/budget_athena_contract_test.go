/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	// The CUR-rollup query contract: it names both activated forms of the PlatformId
	// tag and sums the unblended cost month-to-date.
	//
	// The expression is spelled out here rather than derived, deliberately. This test
	// and curPlatformTagExpr must agree by AGREEING WITH AWS, not with each other —
	// deriving it on both sides would let one wrong spelling pass its own assertion,
	// which is how the previous column name (resource_tags_user_platformid, missing the
	// split inside PlatformId) survived.
	//
	// The column and prefix are the ones the export delivers
	// (terraform/components/cost-pipeline/cur-export-schema.txt), not the ones the
	// dictionary suggests. `tags`, `resourceTags/` and `iamPrincipal/` all read NULL
	// against this export instead of failing, which reported every platform at zero
	// spend and left the kill switch nothing to act on.
	for _, want := range []string{
		"element_at(resource_tags, 'user_PlatformId')",
		") = 'acme'",
		"line_item_unblended_cost",
		"date_trunc('month'",
	} {
		if !strings.Contains(a.lastQuery, want) {
			t.Errorf("CUR rollup query missing %q:\n%s", want, a.lastQuery)
		}
	}
}

// The columns this query names must be columns the CUR table actually declares.
//
// cost-pipeline DECLARES that table rather than discovering it, and it declares a subset
// of the export's 125 CUR 2.0 columns — the ones this org queries. The subset is legal
// because Athena resolves Parquet columns by name, but it means a query naming a column
// outside the subset is not a mismatch Athena forgives: the query FAILS. reconcileBudget
// records a failed query as unreadable spend rather than as an error and returns before
// the kill-switch block, so the budget holds its last value and a tenant over its cap is
// never stopped — with nothing red anywhere.
//
// This reads the Terraform source because that file is the only place the table's column
// set exists. The tftest suite in that component can see the table but not this query;
// this package can see the query but has no other view of the table. Asserting it on both
// sides against two hand-maintained lists is how the two drift apart while each passes its
// own test.
func TestCurRollupQueryOnlyNamesColumnsTheTableDeclares(t *testing.T) {
	const decl = "../../../terraform/components/cost-pipeline/main.tf"

	src, err := os.ReadFile(decl)
	if err != nil {
		// Fatal, not Skip. A check that quietly stops checking when it cannot find its
		// input is the failure mode this whole test exists to prevent.
		t.Fatalf("cannot read the CUR table declaration at %s: %v", decl, err)
	}

	// Only the CUR table's own columns. The estimates table in the same file declares its
	// own, and merging the two would let this pass on a column the CUR table never had.
	block := regexp.MustCompile(`(?s)resource "aws_glue_catalog_table" "cur" \{.*?\n\}\n`).Find(src)
	if block == nil {
		t.Fatalf("no aws_glue_catalog_table.cur block found in %s — the CUR table is declared there, so either it moved or this test can no longer see it", decl)
	}

	// name -> type, captured as a PAIR. Collecting names and types separately and then
	// asking whether the block contains each is not the same assertion: `product` is also
	// a map, so "some column is a map" stays true with `tags` demoted to a string. That
	// exact mutation went uncaught by the first version of this check.
	declared := map[string]string{}
	for _, m := range regexp.MustCompile(`columns \{\s*name\s*=\s*"([a-z_0-9]+)"\s*type\s*=\s*"([^"]+)"`).FindAllSubmatch(block, -1) {
		declared[string(m[1])] = string(m[2])
	}
	if len(declared) == 0 {
		t.Fatalf("extracted no columns from the aws_glue_catalog_table.cur block — the extraction is broken, and an empty set makes every assertion below vacuously true")
	}

	a := succeededAthena(curRollupResultSet("1.00"))
	if _, err := budgetReconciler(a).querySpendFromAthena(context.Background(), "acme"); err != nil {
		t.Fatalf("querySpendFromAthena: %v", err)
	}

	// Every CUR column the query names by bare identifier. The tag map is reached through
	// element_at() and is checked separately below.
	named := regexp.MustCompile(`line_item_[a-z_]+`).FindAllString(a.lastQuery, -1)
	if len(named) == 0 {
		t.Fatalf("the CUR rollup query names no line_item column at all:\n%s", a.lastQuery)
	}
	for _, col := range named {
		if _, ok := declared[col]; !ok {
			t.Errorf("the CUR rollup query reads %q, which %s does not declare on the CUR table.\n"+
				"Athena fails the query rather than returning null for an undeclared column, and a failed\n"+
				"query is recorded as unreadable spend. Declare the column there or stop reading it here.\n"+
				"query:\n%s", col, decl, a.lastQuery)
		}
	}

	// The platform attribution goes through the `resource_tags` map. It has to be
	// declared, and declared AS A MAP — element_at() on a string column fails the
	// whole query.
	if got := declared["resource_tags"]; got != "map<string,string>" {
		t.Errorf("the CUR rollup query attributes spend with element_at(resource_tags, ...), so `resource_tags` must be declared "+
			"map<string,string> on the CUR table in %s; got %q", decl, got)
	}

	// The cost column's type is load-bearing in the same way: SUM() over a string fails.
	if got := declared["line_item_unblended_cost"]; got != "double" {
		t.Errorf("the CUR rollup query SUMs line_item_unblended_cost, so it must be declared double in %s; got %q", decl, got)
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

// TestCurTagColumn pins AWS's documented CUR-to-Athena column transform. The cases
// are AWS's own worked examples from the "Column names" section of the Cost and
// Usage Reports user guide, plus the tag this operator actually stamps.
//
// This is the far end of the budget path that nothing else can reach offline: the
// real column name is produced by AWS when it loads the Parquet, so the only thing
// a test here can bind is that the operator applies the same published rule.
func TestCurPlatformTagExpr(t *testing.T) {
	got := curPlatformTagExpr("PlatformId")

	// The column and key prefix the export actually delivers.
	if want := "element_at(resource_tags, 'user_PlatformId')"; got != want {
		t.Errorf("curPlatformTagExpr = %q, want %q — see cur-export-schema.txt", got, want)
	}

	// The spellings that read NULL against this export rather than failing. Each one
	// shipped at some point and each reported zero spend for every platform.
	for _, forbidden := range []string{"resourceTags/", "iamPrincipal/", "element_at(tags,"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("curPlatformTagExpr contains %q, which is absent from the export and resolves to NULL: %q", forbidden, got)
		}
	}

	// element_at, never the subscript. Athena is Trino: resource_tags['missing'] RAISES
	// rather than returning NULL, so the first untagged line item would fail the entire
	// query instead of yielding a row.
	if strings.Contains(got, "resource_tags['") {
		t.Errorf("curPlatformTagExpr uses the map subscript, which raises on a missing key: %q", got)
	}
}

// TestCurPlatformTagExprMatchesTheActivatedTags proves the expression the query filters
// on names the same tag key cost-pipeline asserts is activated in Cost Explorer. An
// activated tag the query does not name, or a named key no tag produces, are the same
// defect: a query that runs, returns zero, and trips no alarm.
func TestCurPlatformTagExprMatchesTheActivatedTags(t *testing.T) {
	if platformIDTagKey != "PlatformId" {
		t.Fatalf("platformIDTagKey = %q — cost-pipeline asserts PlatformId is the activated cost-allocation key; changing one side alone silently zeroes every budget", platformIDTagKey)
	}
	got := curPlatformTagExpr(platformIDTagKey)
	if !strings.Contains(got, "'user_"+platformIDTagKey+"'") {
		t.Errorf("the query would filter on %q, which does not name the activated key %q with the `user_` prefix CUR 2.0 gives it", got, platformIDTagKey)
	}
}

// TestBudgetSpendUnreadable_CountedWhenTheCurLegIsAbsent drives the real reconcile
// body with no Athena configured, which is what a cost-pipeline that failed to apply
// looks like from the operator: no SSM outputs, so AthenaCfg is empty, so the CUR leg
// falls back to zero.
//
// It asserts through reconcileBudget rather than incrementing the counter directly.
// A test that calls Inc() itself proves the metric is registered and nothing about
// whether the production path ever reaches it — deleting the increment would leave
// such a test green, which is the shape this whole change is about.
func TestBudgetSpendUnreadable_CountedWhenTheCurLegIsAbsent(t *testing.T) {
	ctx := context.Background()
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "nocur-acme", Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseReady},
	}
	cl := fake.NewClientBuilder().WithScheme(killSwitchTestScheme(t)).WithObjects(platform).Build()
	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nocur-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: "nocur-acme"}, MonthlyUsd: "100.00",
		},
	}

	// No Athena client and no AthenaCfg — errAthenaNotConfigured.
	r := &BudgetReconciler{Client: cl, RequeueInterval: time.Hour, ClusterName: "staging-platform"}

	before := testutil.ToFloat64(budgetSpendUnreadableTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name))
	if _, err := r.reconcileBudget(ctx, bp); err != nil {
		t.Fatalf("reconcileBudget: %v", err)
	}
	after := testutil.ToFloat64(budgetSpendUnreadableTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name))
	if after != before+1 {
		t.Errorf("a reconcile whose CUR leg is absent must be counted: %v -> %v\n"+
			"    Without it a budget with no cost pipeline at all reads as healthy — the query "+
			"never runs, so nothing errors, and the tenant has no enforcement.", before, after)
	}
}
