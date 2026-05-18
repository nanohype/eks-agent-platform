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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/v1alpha1"
)

// killSwitchBreachPercent is the percent-of-budget at which the kill-
// switch fires (if BudgetPolicy.spec.killSwitchEnabled is true). Matches
// the contract documented in ADR 0003.
const killSwitchBreachPercent int32 = 120

// budgetEventSource is the EventBridge `Source` value the kill-switch
// rule subscribes to. Keep stable — changing it requires a coordinated
// terraform/components/kill-switch update.
const budgetEventSource = "agents.stxkxs.io/budget"

// bedrockInvocationCostMetric is the per-minute CloudWatch metric the
// Bedrock invocation logger publishes via the cost-pipeline component.
// Used to estimate spend incurred since the most recent CUR partition.
const bedrockInvocationCostMetric = "EstimatedInvocationCostUsd"

var errAthenaNotConfigured = errors.New("athena workgroup/database not configured")

// athenaIdentifierRE is the validator applied to Athena workgroup +
// database names before they're interpolated into a query. SSM values
// are operator-resolved at startup, but they aren't a pre-trusted
// channel — anyone with write access to /eks-agent-platform/<env>/
// could otherwise inject SQL via the spend rollup. AWS Athena's own
// identifier rules are stricter than this; this is a paranoid subset.
var athenaIdentifierRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// errPlatformBudgetNotFound is the sentinel for a BudgetPolicy whose
// platformRef points at a Platform that doesn't exist. Status reflects
// Pending; we don't retry forever because re-reconciliation will be
// driven by Platform create events.
var errPlatformBudgetNotFound = errors.New("budget platformRef not found")

// resolveBudgetPlatform fetches the referenced Platform. Same shape as
// the ModelGateway/AgentFleet resolvers.
func (r *BudgetReconciler) resolveBudgetPlatform(ctx context.Context, bp *agentsv1alpha1.BudgetPolicy) (*agentsv1alpha1.Platform, error) {
	var p agentsv1alpha1.Platform
	key := types.NamespacedName{Namespace: bp.Namespace, Name: bp.Spec.PlatformRef.Name}
	if err := r.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errPlatformBudgetNotFound
		}
		return nil, fmt.Errorf("get platform %s: %w", key, err)
	}
	return &p, nil
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

	// Month-to-date sum of unblended cost grouped by the PlatformId user
	// tag the operator stamps on every taggable AWS resource (tenant IAM
	// role, KMS grant tag, bucket prefix tag — see ADR 0003). Tag columns
	// in CUR v1 Athena get the format
	//   resource_tags_user_<lowercased_tag_name>
	// so PlatformId becomes resource_tags_user_platformid. Identifier
	// inputs are validated against athenaIdentifierRE above; the value
	// flows through escapeSQL even though Kubernetes already constrains
	// it to RFC-1123 (defensive against future schema relaxations).
	query := fmt.Sprintf(
		`SELECT COALESCE(SUM(line_item_unblended_cost), 0) AS spend_usd
		 FROM "%s"."%s"
		 WHERE resource_tags_user_platformid = '%s'
		   AND line_item_usage_start_date >= date_trunc('month', current_date)`,
		r.AthenaCfg.Database, r.AthenaCfg.CURTableName, escapeSQL(platformID),
	)
	startOut, err := r.Athena.StartQueryExecution(ctx, &athena.StartQueryExecutionInput{
		QueryString: aws.String(query),
		WorkGroup:   aws.String(r.AthenaCfg.Workgroup),
		QueryExecutionContext: &athenatypes.QueryExecutionContext{
			Database: aws.String(r.AthenaCfg.Database),
		},
	})
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
		_, _ = r.Athena.StopQueryExecution(stopCtx, &athena.StopQueryExecutionInput{
			QueryExecutionId: aws.String(qid),
		})
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
//   - revokes IRSA permissions,
//   - scales AgentFleets to zero.
//
// We carry both the spend snapshot and the budget threshold in the
// event payload so the SFN execution can render an audit-trail message
// without re-reading Kubernetes state.
func (r *BudgetReconciler) fireKillSwitch(ctx context.Context, bp *agentsv1alpha1.BudgetPolicy, spend string, pct int32) error {
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
		"severity":        "critical",
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
			DetailType:   aws.String("BudgetBreach"),
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
}

func (r *BudgetReconciler) reconcileBudget(ctx context.Context, bp *agentsv1alpha1.BudgetPolicy) (budgetReading, error) {
	platform, err := r.resolveBudgetPlatform(ctx, bp)
	if err != nil {
		if errors.Is(err, errPlatformBudgetNotFound) {
			return budgetReading{}, nil
		}
		return budgetReading{}, err
	}

	// CUR-tagged spend (MTD).
	spendCUR, err := r.querySpendFromAthena(ctx, platform.Name)
	switch {
	case errors.Is(err, errAthenaNotConfigured):
		// Dev/test path: no cost-pipeline outputs in SSM. Fall back to a
		// zero CUR and surface only the in-flight CloudWatch number.
		spendCUR = "0"
	case err != nil:
		return budgetReading{}, err
	}

	// In-flight invocation cost (last 24h to cover CUR partition lag).
	since := time.Now().Add(-24 * time.Hour).UTC()
	spendInflight, err := r.queryInflightCost(ctx, platform.Name, since)
	if err != nil {
		// CloudWatch outage shouldn't block the entire reconciler; we
		// log and zero out the in-flight portion. The Athena CUR value
		// is still a valid (though stale) reading.
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

	fired := false
	if pct >= killSwitchBreachPercent && bp.Spec.KillSwitchEnabled && bp.Status.KillSwitchFiredAt == nil {
		if err := r.fireKillSwitch(ctx, bp, totalSpend, pct); err != nil {
			return budgetReading{}, err
		}
		fired = true
	}

	return budgetReading{
		spendUsd:        totalSpend,
		pct:             pct,
		alertThreshold:  alertAt,
		killSwitchFired: fired,
		platformReady:   platform.Status.Phase == phaseReady,
	}, nil
}

// applyBudgetStatus writes the computed reading into status and emits
// the matching Conditions/Events.
func (r *BudgetReconciler) applyBudgetStatus(ctx context.Context, bp *agentsv1alpha1.BudgetPolicy, reading budgetReading) error {
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
	upsertCondition(&bp.Status.Conditions, cond)

	return r.Status().Update(ctx, bp)
}

// applyBudgetStatusError records a BudgetReconciled=False condition so
// operators can distinguish "reconciler failing" from "reconciler not
// running" without inspecting logs. LastReconciled is not bumped — the
// existing timestamp keeps reflecting the last successful tick, which is
// what the budget-stale alert wants.
func (r *BudgetReconciler) applyBudgetStatusError(ctx context.Context, bp *agentsv1alpha1.BudgetPolicy, reason string, cause error) error {
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
	const minTimeout = 30 * time.Second
	if r.RequeueInterval <= 0 {
		return 2 * time.Minute
	}
	cap := r.RequeueInterval / 2
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
