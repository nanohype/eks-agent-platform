/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	eventbridgetypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
	"github.com/nanohype/eks-agent-platform/operators/internal/awsclients"
)

// killSwitchBreachPercent is the percent-of-budget at which the kill-
// switch fires (if BudgetPolicy.spec.killSwitchEnabled is true). Matches
// the contract documented in ADR 0003.
const killSwitchBreachPercent int32 = 120

// budgetEventSource, budgetEventDetailType, and budgetEventSeverity are the
// exact EventBridge match fields the kill-switch rule
// (terraform/components/kill-switch/main.tf) subscribes to. EventBridge
// matching is exact, so changing any of them on this side without the
// terraform side dead-ends the kill-switch. Keep stable — the seam is pinned
// by budget_killswitch_contract_test.go, which fails the build on drift.
const (
	budgetEventSource     = "governance.nanohype.dev/budget"
	budgetEventDetailType = "BudgetBreach"
	budgetEventSeverity   = "critical"
)

// killSwitch grace/backoff defaults. Fields on BudgetReconciler override the
// grace-interval count and re-fire cap; these are the fallbacks and the
// backoff ceiling.
const (
	killSwitchDefaultGraceIntervals = 3
	killSwitchDefaultMaxRefires     = 5
	killSwitchMaxRefireBackoff      = 6 * time.Hour
)

// bedrockInvocationCostMetric is the per-minute CloudWatch metric the
// Bedrock invocation logger publishes via the cost-pipeline component.
// Used to estimate spend incurred since the most recent CUR partition.
const bedrockInvocationCostMetric = "EstimatedInvocationCostUsd"

var errAthenaNotConfigured = errors.New("athena workgroup/database not configured")

// athenaIdentifierRE is the validator applied to Athena workgroup +
// database names before they're interpolated into a query. SSM values
// are operator-resolved at startup, but they aren't a pre-trusted
// channel — anyone with write access to /eks-agent-platform/<cluster>/
// could otherwise inject SQL via the spend rollup. AWS Athena's own
// identifier rules are stricter than this; this is a paranoid subset.
var athenaIdentifierRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// errPlatformBudgetNotFound is the sentinel for a BudgetPolicy whose
// platformRef points at a Platform that doesn't exist. Status reflects
// Pending; we don't retry forever because re-reconciliation will be
// driven by Platform create events.
var errPlatformBudgetNotFound = errors.New("budget platformRef not found")

// resolveBudgetPlatform fetches the referenced Platform, mapping a dangling ref
// to errPlatformBudgetNotFound (Pending, not a hard error). Shares the fetch
// with the other workload reconcilers via getReferencedPlatform.
func (r *BudgetReconciler) resolveBudgetPlatform(ctx context.Context, bp *governancev1alpha1.BudgetPolicy) (*platformv1alpha1.Platform, error) {
	return getReferencedPlatform(ctx, r.Client, bp.Namespace, bp.Spec.PlatformRef.Name, errPlatformBudgetNotFound)
}

// platformIDTagKey is the cost-allocation tag the operator stamps on every taggable
// tenant resource, and the one cost-pipeline activates in Cost Explorer. Case
// matters: Cost Explorer treats tag keys as case-sensitive.
const platformIDTagKey = "PlatformId"

// platformCostID is the value behind platformIDTagKey: the identity every cost
// signal attributes spend to.
//
// It is cluster-qualified, and that qualification is load-bearing rather than
// decorative. A CUR covers a whole AWS account and a CloudWatch namespace is
// account+region global, while the platform contract explicitly allows two
// co-located clusters to host a Platform of the same name (AGENTS.md, and
// tenantRoleName is cluster-keyed for exactly that reason). An attribution key of
// the bare Platform name therefore names two different tenants in one account:
// their spend sums, every reader sees the total as its own, and at
// killSwitchBreachPercent one environment's traffic suspends another's Platform.
// nanohype/standards/resource-naming.json states the same rule for names —
// "co-located sibling clusters in one account must not collide, so the cluster
// discriminator is load-bearing here" — and an attribution key is subject to it
// for the same reason a name is.
//
// One function, two callers on purpose: the tag written onto tenant resources and
// the predicate the reconciler queries with are the same expression, so they cannot
// drift into a query that is valid, green and empty. Nothing else may reconstruct
// this value by taking apart a name — that is what the cost publisher used to do,
// and it spent the platform's entire in-flight cost signal on a dimension no reader
// ever queried.
//
// Cluster-qualified because a CUR covers a whole account: the bare Platform name
// names two different tenants the moment two clusters share one, and this value is
// what a CUR row carries.
func platformCostID(clusterName, platformName string) string {
	return clusterName + "-" + platformName
}

// curPlatformTagExpr renders the SQL expression that names a line item's platform in a
// CUR 2.0 export.
//
// The column is `resource_tags` and its keys carry a `user_` prefix. Both are read out
// of the delivered export, not out of the AWS dictionary — see
// terraform/components/cost-pipeline/cur-export-schema.txt, which records the columns
// and the observed key shape (user_project, user_business_unit, user_cost_center).
//
// The plausible-but-absent spellings are the hazard here, and there are three of
// them:
//
//	element_at(tags, 'resourceTags/<key>')     no `tags` column, no `resourceTags/` prefix
//	element_at(tags, 'iamPrincipal/<key>')     no `iamPrincipal/` prefix either
//	resource_tags_user_<key>                   CUR 1.0 flattening, not present in CUR 2.0
//
// Each reads naturally off the AWS documentation, and each fails the same silent way.
// Athena resolves Parquet by name with parquet.column.index.access=false, so an absent
// column yields NULL rather than an error: the WHERE clause matches no row, SUM returns
// nothing, and COALESCE(SUM(...), 0) reports 0. Every platform's month-to-date spend
// reads zero, which is a number the kill switch can never act on.
//
// cost-pipeline composes the same expression in HCL. Deriving each side independently
// does not protect against this — independence does not help when the shared input is a
// false premise about the data — so both sides are checked against the recorded export
// instead of against each other.
//
// element_at rather than resource_tags['...']: Athena is Trino, where the map subscript
// operator RAISES on a missing key instead of returning NULL, so the first untagged line
// item would fail the whole query rather than yield a row.
//
// SCOPE, stated rather than implied: this reaches resource-tagged spend. A Bedrock
// invocation is not a taggable resource, and where an activated iamPrincipal/<key> lands
// in a CUR 2.0 export is unverified — Cost Explorer lists such keys in this account but
// all are Inactive, so no delivered row carries one to read. cur-export-schema.txt
// records that as UNVERIFIED instead of guessing a second COALESCE branch, because a
// guessed branch resolves to NULL and reads exactly like a correct one.
func curPlatformTagExpr(tagKey string) string {
	return fmt.Sprintf("element_at(resource_tags, 'user_%s')", tagKey)
}

// inflightWindowStart returns the earliest instant the in-flight CloudWatch leg may
// count from: 24h back to cover CUR's partition lag, clamped to the start of the
// billing period the CUR leg measures.
//
// Both legs feed one total compared against a monthly budget, so the in-flight
// window may never extend before the month the CUR query begins at. Kept as its own
// function because the clamp is the whole point and an inline expression invites
// somebody to "simplify" it back to a bare subtraction.
func inflightWindowStart(now time.Time) time.Time {
	now = now.UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if lookback := now.Add(-24 * time.Hour); lookback.After(monthStart) {
		return lookback
	}
	return monthStart
}

// querySpendFromAthena runs the CUR rollup query for the current
// billing-period MTD and returns the spend (decimal USD as string,
// preserving precision) for the given PlatformId tag.
//
// Athena is asynchronous: StartQueryExecution → poll GetQueryExecution
// until SUCCEEDED/FAILED → GetQueryResults. We cap polling at queryTimeout.
func (r *BudgetReconciler) querySpendFromAthena(ctx context.Context, platformID string) (string, error) {
	if r.Athena == nil {
		return "", errAthenaNotConfigured
	}
	if r.AthenaCfg.Workgroup == "" || r.AthenaCfg.Database == "" || r.AthenaCfg.CURTableName == "" {
		return "", errAthenaNotConfigured
	}
	if !athenaIdentifierRE.MatchString(r.AthenaCfg.Database) {
		return "", fmt.Errorf("athena database name %q failed validation; refusing to build query", r.AthenaCfg.Database)
	}
	if !athenaIdentifierRE.MatchString(r.AthenaCfg.Workgroup) {
		return "", fmt.Errorf("athena workgroup name %q failed validation; refusing to build query", r.AthenaCfg.Workgroup)
	}
	if !athenaIdentifierRE.MatchString(r.AthenaCfg.CURTableName) {
		return "", fmt.Errorf("athena CUR table name %q failed validation; refusing to build query", r.AthenaCfg.CURTableName)
	}

	// Month-to-date sum of unblended cost grouped by the PlatformId user tag the
	// operator stamps on every taggable AWS resource (tenant IAM role, bucket
	// prefix tag — see ADR 0003). The column name is DERIVED from the
	// tag key rather than written out, because the two have to agree and only one
	// of them is ours: AWS renames CUR columns on the way into Athena, and a
	// hand-copied name that drifts from that transform produces a query which is
	// valid, runs, and returns nothing.
	//
	// Identifier inputs are validated against athenaIdentifierRE above; the value
	// flows through escapeSQL even though Kubernetes already constrains it to
	// RFC-1123 (defensive against future schema relaxations).
	//
	// line_item_type = 'Usage' makes this a GROSS CONSUMPTION measure, not a
	// net-billed one. A CUR carries Credit, Refund, Tax, RIFee, SavingsPlanNegation
	// and discount rows alongside usage, and summing them all answers "what was this
	// account charged" — a reasonable question, and the wrong one for a kill switch.
	// A promotional credit would offset real consumption and hold a runaway tenant
	// under its cap; tax would inflate a tenant's number for something it did not
	// cause. The switch exists to stop consumption, so it counts consumption.
	//
	// There is deliberately NO product predicate. A BudgetPolicy caps monthly spend
	// per Platform, which includes the tenant's datastores — the reconciliation view
	// in cost-pipeline filters to Bedrock because it reconciles invocation estimates
	// against Bedrock billing, which is a different question. Do not align them.
	query := fmt.Sprintf(
		`SELECT COALESCE(SUM(line_item_unblended_cost), 0) AS spend_usd
		 FROM "%s"."%s"
		 WHERE %s = '%s'
		   AND line_item_line_item_type = 'Usage'
		   AND line_item_usage_start_date >= date_trunc('month', current_date)`,
		r.AthenaCfg.Database, r.AthenaCfg.CURTableName,
		curPlatformTagExpr(platformIDTagKey), escapeSQL(platformID),
	)
	startOut, err := r.Athena.StartQueryExecution(ctx, &athena.StartQueryExecutionInput{
		QueryString: aws.String(query),
		WorkGroup:   aws.String(r.AthenaCfg.Workgroup),
		QueryExecutionContext: &athenatypes.QueryExecutionContext{
			Database: aws.String(r.AthenaCfg.Database),
		},
		// Exactly one attempt. Every other AWS call this operator makes is
		// declarative and converges on a retry; this one creates a billable
		// scan and hands back an id the caller then tracks. A retried attempt
		// produces a second real query, the caller keeps only the last id, and
		// the earlier scan runs to completion with nothing holding its id to
		// stop it — the deferred StopQueryExecution below can only cancel the
		// one it knows about. The SDK's retry is invisible from here, so the
		// duplicate surfaces on an invoice rather than in a log.
		//
		// Losing the query to a throttle is the safe direction: the reconciler
		// reports the spend unreadable and increments
		// agents_budget_spend_unreadable_total, which is the signal that exists
		// for exactly this. PutEvents on the kill-switch path deliberately KEEPS
		// its retries for the mirrored reason — a duplicate breach event detaches
		// an already-detached policy, while a lost one leaves a tenant spending.
	}, awsclients.NoRetry)
	if err != nil {
		return "", fmt.Errorf("athena StartQueryExecution: %w", err)
	}
	qid := aws.ToString(startOut.QueryExecutionId)

	// Stop the query if we exit the poll loop without consuming the
	// result. Athena charges per scanned byte regardless of whether
	// we read the output — leaving an orphan query running on
	// controller shutdown or poll timeout would bleed money.
	var completed bool
	defer func() {
		if completed {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Best-effort, but not silent. This stop is the only thing between an
		// abandoned query and an open-ended bill, so the case where it FAILS is
		// exactly the case an operator needs to hear about — and the query id is
		// the one thing they need to cancel it by hand. Discarding the error
		// threw that away at the moment it became useful, leaving a scan running
		// with nobody aware and no id to reach it by.
		if _, stopErr := r.Athena.StopQueryExecution(stopCtx, &athena.StopQueryExecutionInput{
			QueryExecutionId: aws.String(qid),
		}); stopErr != nil {
			log.FromContext(ctx).Error(stopErr, "could not stop an abandoned Athena query; it keeps scanning and keeps billing until it completes",
				"queryExecutionId", qid, "remedy", "aws athena stop-query-execution --query-execution-id "+qid)
		}
	}()

	deadline := time.Now().Add(r.queryTimeout())
	for time.Now().Before(deadline) {
		getOut, err := r.Athena.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{
			QueryExecutionId: aws.String(qid),
		})
		if err != nil {
			return "", fmt.Errorf("athena GetQueryExecution %s: %w", qid, err)
		}
		state := getOut.QueryExecution.Status.State
		switch state {
		case athenatypes.QueryExecutionStateSucceeded:
			completed = true
			return r.fetchAthenaResultDecimal(ctx, qid)
		case athenatypes.QueryExecutionStateFailed, athenatypes.QueryExecutionStateCancelled:
			completed = true // already terminal — no Stop needed
			reason := aws.ToString(getOut.QueryExecution.Status.StateChangeReason)
			return "", fmt.Errorf("athena query %s ended in %s: %s", qid, state, reason)
		}
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("athena query %s timed out after %s", qid, r.queryTimeout())
}

// fetchAthenaResultDecimal reads the first result row's spend column.
// Returns "0" when the query produced no rows (a Platform with zero
// activity this month) so callers can carry on without special-casing.
func (r *BudgetReconciler) fetchAthenaResultDecimal(ctx context.Context, queryID string) (string, error) {
	out, err := r.Athena.GetQueryResults(ctx, &athena.GetQueryResultsInput{
		QueryExecutionId: aws.String(queryID),
	})
	if err != nil {
		return "", fmt.Errorf("athena GetQueryResults %s: %w", queryID, err)
	}
	rows := out.ResultSet.Rows
	if len(rows) <= 1 {
		// row 0 is the header
		return "0", nil
	}
	cells := rows[1].Data
	if len(cells) == 0 || cells[0].VarCharValue == nil {
		return "0", nil
	}
	return aws.ToString(cells[0].VarCharValue), nil
}

// queryInflightCost returns the in-flight Bedrock invocation cost since
// the last CUR partition's commit time (~24h lag). CloudWatch reports a
// per-minute Sum of EstimatedInvocationCostUsd dimensioned by PlatformId;
// we GetMetricData over the recent window and Sum into a single decimal.
//
// Returns "0" when CloudWatch is unconfigured or returns no datapoints.
func (r *BudgetReconciler) queryInflightCost(ctx context.Context, platformID string, since time.Time) (string, error) {
	if r.CloudWatch == nil {
		return "0", nil
	}
	end := time.Now().UTC()
	if !since.Before(end) {
		return "0", nil
	}
	out, err := r.CloudWatch.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(since),
		EndTime:   aws.Time(end),
		MetricDataQueries: []cloudwatchtypes.MetricDataQuery{{
			Id: aws.String("inflight"),
			MetricStat: &cloudwatchtypes.MetricStat{
				Metric: &cloudwatchtypes.Metric{
					Namespace:  aws.String("agents/Bedrock"),
					MetricName: aws.String(bedrockInvocationCostMetric),
					Dimensions: []cloudwatchtypes.Dimension{{
						Name:  aws.String("PlatformId"),
						Value: aws.String(platformID),
					}},
				},
				Period: aws.Int32(60),
				Stat:   aws.String("Sum"),
			},
			ReturnData: aws.Bool(true),
		}},
	})
	if err != nil {
		return "0", fmt.Errorf("cloudwatch GetMetricData: %w", err)
	}
	if len(out.MetricDataResults) == 0 {
		return "0", nil
	}
	total := new(big.Float).SetPrec(64)
	for _, v := range out.MetricDataResults[0].Values {
		total.Add(total, big.NewFloat(v))
	}
	return total.Text('f', 6), nil
}

// addDecimal returns a + b with at most 6 fractional digits, using
// big.Float so we don't lose precision on common CUR values (4-6 decimal
// places of dollars).
func addDecimal(a, b string) (string, error) {
	af, _, err := big.ParseFloat(a, 10, 64, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("parse decimal %q: %w", a, err)
	}
	bf, _, err := big.ParseFloat(b, 10, 64, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("parse decimal %q: %w", b, err)
	}
	sum := new(big.Float).SetPrec(64).Add(af, bf)
	return sum.Text('f', 6), nil
}

// percentOfBudget returns int32(round(spend / monthly * 100)). Capped at
// math.MaxInt32 if the user set a microscopic monthly to "verify
// breach". monthly == 0 → 0% (degenerate input; KillSwitch never fires).
func percentOfBudget(spend, monthly string) (int32, error) {
	spendF, _, err := big.ParseFloat(spend, 10, 64, big.ToNearestEven)
	if err != nil {
		return 0, fmt.Errorf("parse spend %q: %w", spend, err)
	}
	monthlyF, _, err := big.ParseFloat(monthly, 10, 64, big.ToNearestEven)
	if err != nil {
		return 0, fmt.Errorf("parse monthly %q: %w", monthly, err)
	}
	if monthlyF.Sign() == 0 {
		return 0, nil
	}
	ratio := new(big.Float).SetPrec(64).Quo(spendF, monthlyF)
	ratio.Mul(ratio, big.NewFloat(100))
	pctF, _ := ratio.Float64()
	if pctF < 0 {
		return 0, nil
	}
	// Round to nearest integer.
	pctI := int64(pctF + 0.5)
	const maxPct = int64(2_000_000_000)
	if pctI > maxPct {
		pctI = maxPct
	}
	return int32(pctI), nil
}

// shouldAlertAt returns the highest threshold the current pct has crossed
// that we haven't already announced (compared to the last value in
// status.percentOfBudget). Returns 0 when no new threshold has been
// crossed since the last reconcile.
//
// When currentPct is strictly less than lastPct we treat lastPct as 0
// for the comparison. Otherwise a billing-period reset (or a CUR
// correction) would permanently suppress every threshold below the
// historic peak — e.g. spend goes 90% → 40% → 60% and the 50% alert
// never fires.
func shouldAlertAt(thresholds []int32, lastPct, currentPct int32) int32 {
	if currentPct < lastPct {
		lastPct = 0
	}
	if len(thresholds) == 0 || currentPct <= lastPct {
		return 0
	}
	sorted := append([]int32(nil), thresholds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var crossed int32
	for _, t := range sorted {
		if t > lastPct && t <= currentPct {
			crossed = t
		}
	}
	return crossed
}

// fireKillSwitch publishes a BudgetBreach event to the kill-switch
// EventBridge bus. The terraform-managed bus has a rule that targets the
// suspension Step Functions state machine, which:
//   - flips Platform.status.phase to Suspended,
//   - revokes the tenant role's permissions,
//   - scales AgentFleets to zero.
//
// We carry both the spend snapshot and the budget threshold in the
// event payload so the SFN execution can render an audit-trail message
// without re-reading Kubernetes state.
func (r *BudgetReconciler) fireKillSwitch(ctx context.Context, bp *governancev1alpha1.BudgetPolicy, spend string, pct int32) error {
	if r.EventBridge == nil || r.KillSwitchEventBusName == "" {
		// No bus configured → log-only mode. The status condition already
		// records the breach; ops alerting can fire from there.
		return nil
	}
	detail := map[string]any{
		"platformId":      bp.Spec.PlatformRef.Name,
		"namespace":       bp.Namespace,
		"budgetPolicy":    bp.Name,
		"monthlyUsd":      bp.Spec.MonthlyUsd,
		"currentSpendUsd": spend,
		"percentOfBudget": pct,
		"severity":        budgetEventSeverity,
		"reason":          "budget-exceeded",
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshal kill-switch event: %w", err)
	}
	out, err := r.EventBridge.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []eventbridgetypes.PutEventsRequestEntry{{
			EventBusName: aws.String(r.KillSwitchEventBusName),
			Source:       aws.String(budgetEventSource),
			DetailType:   aws.String(budgetEventDetailType),
			Detail:       aws.String(string(payload)),
			Time:         aws.Time(time.Now().UTC()),
		}},
	})
	if err != nil {
		return fmt.Errorf("eventbridge PutEvents: %w", err)
	}
	// PutEvents returns HTTP 200 even when individual entries fail
	// (bad bus name, IAM denial, throttling) — partial failures are
	// signaled via FailedEntryCount + per-entry ErrorCode. Treat any
	// failed entry as a real error so the kill-switch can be retried
	// on the next reconcile instead of silently dropping the breach.
	if out.FailedEntryCount > 0 {
		code := ""
		msg := ""
		if len(out.Entries) > 0 {
			code = aws.ToString(out.Entries[0].ErrorCode)
			msg = aws.ToString(out.Entries[0].ErrorMessage)
		}
		return fmt.Errorf("eventbridge PutEvents partial failure: %d entries failed (%s: %s)", out.FailedEntryCount, code, msg)
	}
	return nil
}

// reconcileBudget is the substantive body. Returns the values to write
// to status (spend, pct, killSwitchFiredAt-or-nil) + the alert threshold
// we want to record as a condition, if any.
type budgetReading struct {
	spendUsd        string
	pct             int32
	alertThreshold  int32
	killSwitchFired bool
	platformReady   bool

	// Effect-verification signals for an already-fired kill-switch.
	killSwitchActive   bool // KillSwitchFiredAt is (or becomes) set this tick
	killSwitchRefired  bool // the breach event was re-published this tick
	killSwitchUnrouted bool // fired, grace elapsed, platform still not Suspended
	platformSuspended  bool // platform observed in the Suspended phase
}

// killSwitchEffect is the decision the reconciler reaches about an
// already-fired kill-switch given the platform's observed suspension state.
// Publishing an event is not success; the platform being Suspended is. When
// it isn't, we re-publish on a bounded exponential backoff instead of
// latching on a false "published" success.
type killSwitchEffect struct {
	unrouted bool // grace elapsed and the platform is still not Suspended
	refire   bool // re-publish the breach event this tick
}

// killSwitchGraceWindow is how long after firing the reconciler waits before
// declaring an un-suspended platform "unrouted": KillSwitchGraceIntervals ×
// RequeueInterval, defaulting to 3 × the tick.
func (r *BudgetReconciler) killSwitchGraceWindow() time.Duration {
	n := r.KillSwitchGraceIntervals
	if n <= 0 {
		n = killSwitchDefaultGraceIntervals
	}
	interval := r.RequeueInterval
	if interval <= 0 {
		interval = time.Hour
	}
	return time.Duration(n) * interval
}

// killSwitchMaxRefires is the bounded number of re-publishes for one breach.
func (r *BudgetReconciler) killSwitchMaxRefires() int {
	if r.KillSwitchMaxRefires <= 0 {
		return killSwitchDefaultMaxRefires
	}
	return r.KillSwitchMaxRefires
}

// killSwitchRefireBackoff returns the minimum wait before the next re-publish:
// the grace window doubled once per prior re-fire, capped at
// killSwitchMaxRefireBackoff so backoff can't run away.
func (r *BudgetReconciler) killSwitchRefireBackoff(refireCount int32) time.Duration {
	d := r.killSwitchGraceWindow()
	for i := int32(0); i < refireCount; i++ {
		d *= 2
		if d >= killSwitchMaxRefireBackoff {
			return killSwitchMaxRefireBackoff
		}
	}
	if d > killSwitchMaxRefireBackoff {
		return killSwitchMaxRefireBackoff
	}
	return d
}

// killSwitchEffect evaluates the effect-verification state for a BudgetPolicy
// whose kill-switch has already fired. It is pure over its inputs so the
// grace/backoff/bound logic is unit-testable without a cluster.
func (r *BudgetReconciler) killSwitchEffect(bp *governancev1alpha1.BudgetPolicy, platformSuspended bool, now time.Time) killSwitchEffect {
	if bp.Status.KillSwitchFiredAt == nil {
		// Never fired — nothing to verify.
		return killSwitchEffect{}
	}
	if platformSuspended {
		// Effect confirmed; the latch settles.
		return killSwitchEffect{}
	}
	if now.Sub(bp.Status.KillSwitchFiredAt.Time) < r.killSwitchGraceWindow() {
		// Still inside the grace window; give the state machine time to run.
		return killSwitchEffect{}
	}
	eff := killSwitchEffect{unrouted: true}
	if int(bp.Status.KillSwitchRefireCount) >= r.killSwitchMaxRefires() {
		// Bounded: stop re-publishing, but keep reporting unrouted so the
		// alert stays lit until a human intervenes.
		return eff
	}
	anchor := bp.Status.KillSwitchFiredAt.Time
	if bp.Status.KillSwitchLastRefireAt != nil {
		anchor = bp.Status.KillSwitchLastRefireAt.Time
	}
	if now.Sub(anchor) >= r.killSwitchRefireBackoff(bp.Status.KillSwitchRefireCount) {
		eff.refire = true
	}
	return eff
}

func (r *BudgetReconciler) reconcileBudget(ctx context.Context, bp *governancev1alpha1.BudgetPolicy) (budgetReading, error) {
	platform, err := r.resolveBudgetPlatform(ctx, bp)
	if err != nil {
		if errors.Is(err, errPlatformBudgetNotFound) {
			return budgetReading{}, nil
		}
		return budgetReading{}, err
	}

	// Every spend signal is keyed on the cluster-qualified identity, which is the
	// same expression stamped onto the tenant's resources as the PlatformId tag.
	//
	// Refuse rather than query without the discriminator. An empty cluster name does
	// not fail on its own — it renders "-acme", which is a perfectly valid predicate
	// that matches no CUR row and no metric series, so every tenant would read zero
	// spend and every budget would look healthy. main.go will not start without a
	// cluster name; this is the second half of that guarantee, at the point where a
	// missing one would actually produce a number.
	if r.ClusterName == "" {
		budgetSpendUnreadableTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name).Inc()
		return budgetReading{}, fmt.Errorf("budget reconciler has no cluster name; refusing to attribute spend without the discriminator that separates same-named Platforms in one account")
	}
	costID := platformCostID(r.ClusterName, platform.Name)

	// CUR-tagged spend (MTD).
	spendCUR, err := r.querySpendFromAthena(ctx, costID)
	switch {
	case errors.Is(err, errAthenaNotConfigured):
		// No cost-pipeline outputs in SSM. Fall back to a zero CUR and surface only
		// the in-flight CloudWatch number — but COUNT it, because this is not only
		// the dev/test path. It is also what a cost-pipeline that failed to apply
		// looks like from here, and that was the actual state: the CUR report
		// definition could not be created, so the component never published its
		// outputs, so this branch ran on every tick for every tenant. Returning a
		// zero without a signal is how a budget with no CUR leg reads as healthy.
		budgetSpendUnreadableTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name).Inc()
		spendCUR = "0"
	case err != nil:
		return budgetReading{}, err
	}

	// In-flight invocation cost — the last 24h, to cover CUR partition lag, but
	// never earlier than the billing period the CUR leg is measuring.
	//
	// The two legs are summed and compared against a MONTHLY budget, and the CUR
	// leg is strictly month-to-date (date_trunc('month', current_date)). An
	// unclamped 24h lookback reaches into the previous month for the first day of
	// every month, so a tenant that spent its budget in January starts February
	// with up to a day of January's spend already counted against February's — and
	// at killSwitchBreachPercent that suspends a Platform on the 1st for money it
	// spent before the period began.
	//
	// This only became reachable when the in-flight leg started returning a number
	// at all; while it read a permanent zero the window could not be wrong.
	since := inflightWindowStart(time.Now().UTC())
	spendInflight, err := r.queryInflightCost(ctx, costID, since)
	if err != nil {
		// CloudWatch outage shouldn't block the entire reconciler; we log
		// and zero out the in-flight portion. The Athena CUR value is still
		// a valid (though stale) reading. The log call makes a persistently
		// failing in-flight query visible — without it the reconciler would
		// silently undercount spend against the budget on every tick.
		log.FromContext(ctx).Error(err, "in-flight CloudWatch cost query failed; zeroing the in-flight spend portion for this tick (CUR-derived spend still applies)",
			"platform", platform.Name, "budgetPolicy", bp.Name)
		spendInflight = "0"
	}

	totalSpend, err := addDecimal(spendCUR, spendInflight)
	if err != nil {
		return budgetReading{}, err
	}
	pct, err := percentOfBudget(totalSpend, bp.Spec.MonthlyUsd)
	if err != nil {
		return budgetReading{}, err
	}

	thresholds := bp.Spec.AlertThresholdsPercent
	alertAt := shouldAlertAt(thresholds, bp.Status.PercentOfBudget, pct)

	platformSuspended := platform.Status.Phase == phaseSuspended

	fired := false
	refired := false
	unrouted := false
	if bp.Spec.KillSwitchEnabled {
		if bp.Status.KillSwitchFiredAt == nil {
			// First breach at/above the kill-switch threshold: publish once.
			if pct >= killSwitchBreachPercent {
				if err := r.fireKillSwitch(ctx, bp, totalSpend, pct); err != nil {
					return budgetReading{}, err
				}
				fired = true
			}
		} else {
			// Already fired. Publishing the event was not the goal — the
			// platform being Suspended is. Verify the effect; if the
			// suspension path is broken, re-publish (bounded) and flag it so
			// the latch can't record a false success.
			eff := r.killSwitchEffect(bp, platformSuspended, time.Now())
			if eff.unrouted {
				unrouted = true
				killSwitchUnroutedTotal.WithLabelValues(bp.Namespace, bp.Name, platform.Name).Inc()
			}
			if eff.refire {
				if err := r.fireKillSwitch(ctx, bp, totalSpend, pct); err != nil {
					return budgetReading{}, err
				}
				refired = true
			}
		}
	}

	return budgetReading{
		spendUsd:           totalSpend,
		pct:                pct,
		alertThreshold:     alertAt,
		killSwitchFired:    fired,
		killSwitchActive:   fired || bp.Status.KillSwitchFiredAt != nil,
		killSwitchRefired:  refired,
		killSwitchUnrouted: unrouted,
		platformSuspended:  platformSuspended,
		platformReady:      platform.Status.Phase == phaseReady,
	}, nil
}

// applyBudgetStatus writes the computed reading into status and emits
// the matching Conditions/Events.
func (r *BudgetReconciler) applyBudgetStatus(ctx context.Context, bp *governancev1alpha1.BudgetPolicy, reading budgetReading) error {
	bp.Status.CurrentSpendUsd = reading.spendUsd
	bp.Status.PercentOfBudget = reading.pct
	now := metav1.Now()
	bp.Status.LastReconciled = &now

	condType := "BudgetReconciled"
	cond := metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("spend=%s usd (%d%% of monthly budget %s)", reading.spendUsd, reading.pct, bp.Spec.MonthlyUsd),
		LastTransitionTime: now,
		ObservedGeneration: bp.Generation,
	}

	if reading.alertThreshold > 0 {
		cond.Reason = "ThresholdCrossed"
		cond.Message = fmt.Sprintf("%s; crossed %d%% alert threshold", cond.Message, reading.alertThreshold)
	}
	if reading.killSwitchFired {
		bp.Status.KillSwitchFiredAt = &now
		cond.Status = metav1.ConditionFalse
		cond.Reason = "KillSwitchFired"
		cond.Message = fmt.Sprintf("budget breach at %d%%; kill-switch event published to %s", reading.pct, r.KillSwitchEventBusName)
	}
	if reading.killSwitchRefired {
		bp.Status.KillSwitchRefireCount++
		bp.Status.KillSwitchLastRefireAt = &now
	}
	upsertCondition(&bp.Status.Conditions, cond)

	// Effect-verification condition: only meaningful once the switch has
	// fired. Tri-state so operators can tell "took effect" from "still
	// routing" from "broken".
	if reading.killSwitchActive {
		r.applyKillSwitchEffectCondition(bp, reading, now)
	}

	return r.Status().Update(ctx, bp)
}

// applyKillSwitchEffectCondition upserts the KillSwitchUnrouted condition
// describing whether a fired kill-switch actually suspended the platform.
//   - SuspensionObserved (False): the platform is Suspended — success.
//   - AwaitingSuspension (False): fired, still inside the grace window.
//   - SuspensionNotObserved (True): grace elapsed and the platform is still
//     not Suspended — the EventBridge→StepFunctions path is broken.
func (r *BudgetReconciler) applyKillSwitchEffectCondition(bp *governancev1alpha1.BudgetPolicy, reading budgetReading, now metav1.Time) {
	cond := metav1.Condition{
		Type:               "KillSwitchUnrouted",
		LastTransitionTime: now,
		ObservedGeneration: bp.Generation,
	}
	switch {
	case reading.platformSuspended:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "SuspensionObserved"
		cond.Message = "kill-switch fired and the platform is observed Suspended"
	case reading.killSwitchUnrouted:
		firedAt := "an earlier tick"
		if bp.Status.KillSwitchFiredAt != nil {
			firedAt = bp.Status.KillSwitchFiredAt.Time.UTC().Format(time.RFC3339)
		}
		cond.Status = metav1.ConditionTrue
		cond.Reason = "SuspensionNotObserved"
		cond.Message = fmt.Sprintf("kill-switch fired at %s but the platform is not Suspended after the grace window; breach re-published %d time(s). The EventBridge→StepFunctions suspension path is likely broken — see the kill-switch runbook.", firedAt, bp.Status.KillSwitchRefireCount)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "AwaitingSuspension"
		cond.Message = "kill-switch fired; awaiting platform suspension within the grace window"
	}
	upsertCondition(&bp.Status.Conditions, cond)
}

// statusWriteTimeout bounds a status write made after the reconcile's own
// context is gone. Short: it is one API-server call on the recovery path, and a
// worker held there is a worker not reconciling anything else.
const statusWriteTimeout = 15 * time.Second

// applyBudgetStatusError records a BudgetReconciled=False condition so
// operators can distinguish "reconciler failing" from "reconciler not
// running" without inspecting logs. LastReconciled is not bumped — the
// existing timestamp keeps reflecting the last successful tick, which is
// what the budget-stale alert wants.
func (r *BudgetReconciler) applyBudgetStatusError(ctx context.Context, bp *governancev1alpha1.BudgetPolicy, reason string, cause error) error {
	// The write that records WHY a reconcile failed must not fail for the same
	// reason. When the cause is the per-reconcile ceiling, ctx is already past
	// its deadline and this update would be refused before it left the process,
	// leaving no condition at all for the failure mode most in need of one. A
	// fresh context with its own short bound, the same shape the Athena
	// cancellation path uses.
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), statusWriteTimeout)
		defer cancel()
	}
	upsertCondition(&bp.Status.Conditions, metav1.Condition{
		Type:               "BudgetReconciled",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: bp.Generation,
	})
	return r.Status().Update(ctx, bp)
}

// queryTimeout returns the deadline cap for an Athena poll loop. Bound
// by the reconciler's RequeueInterval so a stuck query doesn't outlive
// the next tick. Lower bound is 30s to leave room for cold CUR scans.
func (r *BudgetReconciler) queryTimeout() time.Duration {
	return BudgetQueryTimeout(r.RequeueInterval)
}

// BudgetQueryTimeout is the Athena poll bound for a given requeue interval.
//
// Exported because it is the one bound in this operator that is DERIVED rather
// than declared, and a per-reconcile ceiling has to exceed it: a ceiling shorter
// than this cancels the poll every tick, and a cost query that never completes
// is a budget cap that is never enforced while the scan is billed each time.
// The caller that sets the ceiling reads it from here rather than restating the
// formula, so widening the interval widens both together.
func BudgetQueryTimeout(requeueInterval time.Duration) time.Duration {
	const minTimeout = 30 * time.Second
	if requeueInterval <= 0 {
		return 2 * time.Minute
	}
	cap := requeueInterval / 2
	if cap < minTimeout {
		return minTimeout
	}
	return cap
}

// escapeSQL is a minimal single-quote escaper for a value that is already
// constrained by the Kubernetes name validator (RFC 1123: lowercase
// alphanumerics + '-'). The double-up is defensive against future schema
// relaxations.
func escapeSQL(in string) string {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '\'' {
			out = append(out, '\'', '\'')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// sleepCtx sleeps for d or until ctx is canceled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
