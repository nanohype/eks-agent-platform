/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The cost-attribution identity, asserted as a RELATION between the side that
// writes it and the sides that read it.
//
// Spend attribution has four participants: the tag the operator stamps on the
// roles it mints, the predicate its Athena rollup filters CUR on, the dimension
// it queries CloudWatch with, and the tag landing-zone's tenant-substrate puts on
// the datastores that actually bill. All four have to be one string. Nothing
// enforced that, and three separate defects lived in the gap — the publisher
// derived a different value, the reconciler queried a third, and every one of
// them was a query that ran, returned, and was empty.
//
// So these tests do not check that a function returns what it returns. They drive
// the real reconcile path with recording fakes, pull the identity back out of the
// query text and the metric dimension, build the role tags through the real tag
// constructor, and assert the three agree. A change to any one of them alone
// fails here.
//
// The negative assertion is the load-bearing one: the identity must NOT be the
// bare Platform name. A CUR covers a whole account and a CloudWatch namespace is
// account+region global, while two co-located clusters may each host a Platform
// called `acme` (AGENTS.md, and tenantRoleName is cluster-keyed for exactly that
// reason). Bare, those two tenants are one row: their spend sums, both operators
// read the total as their own, and at killSwitchBreachPercent the loser is
// suspended for traffic it never made.

// recordingCloudWatch captures the dimensions of the GetMetricData request so the
// dimension the reconciler actually queries can be asserted, rather than the
// value a test passed into a helper.
type recordingCloudWatch struct {
	dimensions []cloudwatchtypes.Dimension
	namespace  string
	metricName string
}

func (f *recordingCloudWatch) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	if len(in.MetricDataQueries) > 0 && in.MetricDataQueries[0].MetricStat != nil {
		m := in.MetricDataQueries[0].MetricStat.Metric
		f.dimensions = m.Dimensions
		f.namespace = aws.ToString(m.Namespace)
		f.metricName = aws.ToString(m.MetricName)
	}
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: []cloudwatchtypes.MetricDataResult{{
		Id: aws.String("inflight"), Values: []float64{1.5},
	}}}, nil
}

func (f *recordingCloudWatch) dimension(name string) string {
	for _, d := range f.dimensions {
		if aws.ToString(d.Name) == name {
			return aws.ToString(d.Value)
		}
	}
	return ""
}

const (
	identityCluster  = "staging-platform"
	identityPlatform = "acme"
)

func tagValue(tags []iamtypes.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// reconcileForIdentity runs a full budget reconcile against recording fakes and
// returns what the two read paths asked AWS for.
func reconcileForIdentity(t *testing.T) (query string, cw *recordingCloudWatch) {
	t.Helper()
	ctx := context.Background()
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: identityPlatform, Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseReady},
	}
	cl := fake.NewClientBuilder().WithScheme(killSwitchTestScheme(t)).WithObjects(platform).Build()
	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: identityPlatform}, MonthlyUsd: "100.00",
		},
	}

	athena := succeededAthena(curRollupResultSet("12.500000"))
	cw = &recordingCloudWatch{}
	r := &BudgetReconciler{
		Client:          cl,
		Athena:          athena,
		CloudWatch:      cw,
		ClusterName:     identityCluster,
		RequeueInterval: time.Hour,
		AthenaCfg:       AthenaConfig{Workgroup: "cost_wg", Database: "cost_db", CURTableName: "cur_eks_agent_platform"},
	}
	if _, err := r.reconcileBudget(ctx, bp); err != nil {
		t.Fatalf("reconcileBudget: %v", err)
	}
	return athena.lastQuery, cw
}

func TestCostIdentity_EveryReaderAndWriterUsesOneValue(t *testing.T) {
	want := platformCostID(identityCluster, identityPlatform)
	query, cw := reconcileForIdentity(t)

	cfg := IAMConfig{ClusterName: identityCluster, Environment: "staging"}
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: identityPlatform, Namespace: "tenants-acme"},
		Spec:       platformv1alpha1.PlatformSpec{Tenant: "acme-team", Persona: "engineer"},
	}

	// The tag the cost publisher reads back off the invoking role. Both role
	// families are asserted: session roles were returning "unknown" for their
	// entire life because a name-parsing consumer only recognised "-tenant".
	tenantTag := tagValue(tenantRoleTags(platform, cfg), platformIDTagKey)
	sessionTag := tagValue(sessionRoleTags(platform, cfg), platformIDTagKey)

	// The value the CUR predicate filters on, recovered from the query the
	// reconciler actually sent rather than from a helper called twice.
	if !strings.Contains(query, "'"+want+"'") {
		t.Errorf("the CUR predicate must filter on %q\n    query: %s", want, query)
	}

	got := map[string]string{
		"tenant role PlatformId tag":  tenantTag,
		"session role PlatformId tag": sessionTag,
		"CloudWatch PlatformId dim":   cw.dimension("PlatformId"),
	}
	for where, v := range got {
		if v != want {
			t.Errorf("%s is %q, want %q\n"+
				"    Every producer and consumer of the attribution identity must render one\n"+
				"    string. When they disagree the query still runs and still returns — empty —\n"+
				"    so the spend reads zero and the budget reads healthy.", where, v, want)
		}
	}
}

func TestCostIdentity_IsNotTheBarePlatformName(t *testing.T) {
	// The defect this whole relation exists to prevent. A bare name is unique in
	// a cluster and ambiguous in an account, and both of the places it lands —
	// a CUR row and a CloudWatch series — are account-scoped.
	query, cw := reconcileForIdentity(t)
	cfg := IAMConfig{ClusterName: identityCluster, Environment: "staging"}
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: identityPlatform, Namespace: "tenants-acme"},
	}

	if v := tagValue(tenantRoleTags(platform, cfg), platformIDTagKey); v == identityPlatform {
		t.Errorf("the PlatformId tag is the bare Platform name %q\n"+
			"    Two clusters in one account may both host a Platform of this name, and CUR is\n"+
			"    account-wide — so both tenants' datastores would carry an identical tag and\n"+
			"    each operator would read the pair's combined spend as its own.", v)
	}
	if v := cw.dimension("PlatformId"); v == identityPlatform {
		t.Errorf("the CloudWatch dimension is the bare Platform name %q — agents/Bedrock is an\n"+
			"    account+region-global namespace, so same-named Platforms share one series.", v)
	}
	if strings.Contains(query, "'"+identityPlatform+"'") {
		t.Errorf("the CUR predicate filters on the bare Platform name\n    query: %s", query)
	}
	if !strings.HasPrefix(platformCostID(identityCluster, identityPlatform), identityCluster+"-") {
		t.Error("the identity must be cluster-qualified — that qualification is the only thing " +
			"keeping co-located sibling clusters apart in an account-scoped cost signal")
	}
}

func TestCostIdentity_RefusesToAttributeWithoutTheDiscriminator(t *testing.T) {
	// The wiring guard in main.go is one half; this is the other. An empty cluster
	// name does not fail — it renders "-acme", a valid predicate matching no CUR row
	// and no metric series — so without this the reconciler would compute a
	// confident zero for every tenant and every budget would read healthy.
	ctx := context.Background()
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: identityPlatform, Namespace: "tenants-acme"},
		Status:     platformv1alpha1.PlatformStatus{Phase: phaseReady},
	}
	cl := fake.NewClientBuilder().WithScheme(killSwitchTestScheme(t)).WithObjects(platform).Build()
	bp := &governancev1alpha1.BudgetPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-budget", Namespace: "tenants-acme"},
		Spec: governancev1alpha1.BudgetPolicySpec{
			PlatformRef: commonv1alpha1.LocalRef{Name: identityPlatform}, MonthlyUsd: "100.00",
		},
	}
	athena := succeededAthena(curRollupResultSet("12.500000"))
	r := &BudgetReconciler{
		Client: cl, Athena: athena, CloudWatch: &recordingCloudWatch{},
		RequeueInterval: time.Hour,
		AthenaCfg:       AthenaConfig{Workgroup: "cost_wg", Database: "cost_db", CURTableName: "cur_t"},
		// ClusterName deliberately unset.
	}

	before := testutil.ToFloat64(budgetSpendUnreadableTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name))
	if _, err := r.reconcileBudget(ctx, bp); err == nil {
		t.Fatal("a reconciler with no cluster name must refuse rather than query with '-acme'")
	}
	if after := testutil.ToFloat64(budgetSpendUnreadableTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name)); after != before+1 {
		t.Errorf("the refusal must be counted: %v -> %v", before, after)
	}
	if athena.lastQuery != "" {
		t.Errorf("no query may be issued without the discriminator, got: %s", athena.lastQuery)
	}
}

func TestCostIdentity_CloudWatchSeriesIsUnchangedOtherwise(t *testing.T) {
	// The namespace and metric name are a contract with the publisher and with
	// ADR 0005; only the dimension VALUE moved. Pinned so a future edit to the
	// identity cannot quietly relocate the series the publisher writes to.
	_, cw := reconcileForIdentity(t)
	if cw.namespace != "agents/Bedrock" {
		t.Errorf("namespace: got %q want agents/Bedrock", cw.namespace)
	}
	if cw.metricName != bedrockInvocationCostMetric {
		t.Errorf("metric: got %q want %q", cw.metricName, bedrockInvocationCostMetric)
	}
	if len(cw.dimensions) != 1 {
		t.Errorf("the publisher dimensions EstimatedInvocationCostUsd on PlatformId alone; "+
			"querying %d dimensions matches nothing", len(cw.dimensions))
	}
}
