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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	eventbridgetypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// sloEventSource and sloEventDetailType are the exact EventBridge match fields
// the burn-rate rules (terraform/components/kill-switch/burn-rate.tf) subscribe
// to. EventBridge matching is exact, so changing either on this side without the
// terraform side dead-ends the page. Keep stable — the seam is pinned by
// slo_killswitch_contract_test.go, which fails the build on drift.
//
// Both differ from the budget breach's source and detail-type, which is what
// guarantees the two rule sets on the shared bus cannot cross-match: a burn-rate
// breach holds a tenant's delivery, it must never reach the suspension state
// machine that detaches IAM. The contract test asserts the disjointness too, so
// a future rename that collapses them fails the build rather than production.
const (
	sloEventSource     = "governance.nanohype.dev/slo"
	sloEventDetailType = "BurnRateBreach"
)

// Severity tiers, matching the observability-slo standard's fleet_alerting tier
// ids — which is also what routes the event: critical pages, warning tickets.
const (
	severityCritical = "critical"
	severityWarning  = "warning"
)

// Metric label values for the two tiers.
const (
	tierPage   = "page"
	tierTicket = "ticket"
)

// burnWindow is one multi-window multi-burn-rate check.
//
// Transcribed from nanohype/standards/observability-slo.json →
// content.burn_rate_alerts.windows. The standard is the single source of truth
// for these numbers; nanohype's check-slo-constants gate reads this table and
// fails if it drifts, the same way it already pins the eks-gitops alert exprs.
// Do not tune a factor here — change the standard.
//
// A pair trips only when BOTH windows exceed the factor: the long window is the
// signal, the short window is the confirmation that the burn is still happening
// now. That is what suppresses a flap and a spike that already healed.
type burnWindow struct {
	severity string
	long     string
	short    string
	factor   float64
}

var burnWindows = []burnWindow{
	{severity: severityCritical, long: "1h", short: "5m", factor: 14.4},
	{severity: severityCritical, long: "6h", short: "30m", factor: 6},
	{severity: severityWarning, long: "1d", short: "2h", factor: 3},
	{severity: severityWarning, long: "3d", short: "6h", factor: 1},
}

// sloWindow is the SLO's own rolling window (observability-slo →
// content.slo.window_days = 30). The remaining error budget is measured over it
// directly rather than extrapolated from a burn window, so the gauge means what
// it says.
const sloWindow = "30d"

// sliTypeAvailability and sliTypeLatency mirror the standard's slo.sli_types ids
// and the CRD's enum.
const (
	sliTypeAvailability = "availability"
	sliTypeLatency      = "latency"
)

// onBreachHoldRollout is the SLOPolicy spec value that opts into the automated
// rollout hold.
const onBreachHoldRollout = "HoldRollout"

// sliDenominatorFloor clamps the valid-events denominator so a service with
// almost no traffic records a zero error ratio instead of NaN. Copied from the
// operator's own SLO recording rules, which clamp at the same value for the same
// reason (charts/operator/files/slo/prometheusrule.yaml). It floors at one
// request per ~17 minutes, so it only engages for a genuinely idle service —
// where a single failed request would otherwise read as a 100% burn.
const sliDenominatorFloor = "0.001"

var (
	errAMPNotConfigured = errors.New("amp query endpoint not configured")
	// errPlatformSLONotFound is the sentinel for an SLOPolicy whose platformRef
	// points at a Platform that doesn't exist. Reported as a condition, not a
	// hard error — re-reconciliation is driven by the next tick.
	errPlatformSLONotFound = errors.New("slopolicy platformRef not found")
)

// promIdentifierRE validates a metric or label name before it is interpolated
// into a PromQL expression. The CRD already constrains spec.sli.metric with the
// same shape, but the query is signed with the operator's own credentials and
// sent to the platform's metric store, so the check is repeated here rather than
// trusted from the API server — the same posture athenaIdentifierRE takes for
// the CUR rollup.
var promIdentifierRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// promDecimalRE validates a decimal literal (a latency threshold) before
// interpolation.
var promDecimalRE = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,6})?$`)

// promDurationRE matches the Prometheus duration literals this reconciler uses.
// Windows come from the burnWindows table, never from user input, so this guards
// a future edit rather than a caller.
var promDurationRE = regexp.MustCompile(`^[0-9]+(ms|s|m|h|d|w|y)$`)

// reservedSelectorLabels are label names a Selector may not set. le is owned by
// the latency query's bucket boundary, and __name__ would let a selector
// redirect the query at an entirely different series.
var reservedSelectorLabels = map[string]bool{"le": true, "__name__": true}

// resolveSLOPlatform fetches the referenced Platform, mapping a dangling ref to
// errPlatformSLONotFound.
func (r *SLOReconciler) resolveSLOPlatform(ctx context.Context, sp *governancev1alpha1.SLOPolicy) (*platformv1alpha1.Platform, error) {
	return getReferencedPlatform(ctx, r.Client, sp.Namespace, sp.Spec.PlatformRef.Name, errPlatformSLONotFound)
}

// escapePromLabelValue quotes a label value for a PromQL matcher. PromQL uses
// Go-style escapes inside double quotes, so a backslash, a quote, or a newline
// in a value would otherwise terminate or extend the matcher.
func escapePromLabelValue(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for i := 0; i < len(in); i++ {
		switch c := in[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// selectorMatchers renders a Selector as sorted exact-match PromQL matchers.
// Sorted so the generated query is stable across reconciles — an unstable query
// string would make every logged expression diff pure noise.
func selectorMatchers(sel map[string]string) ([]string, error) {
	if len(sel) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if reservedSelectorLabels[k] {
			return nil, fmt.Errorf("selector label %q is reserved and cannot be set", k)
		}
		if !promIdentifierRE.MatchString(k) {
			return nil, fmt.Errorf("selector label %q is not a valid Prometheus label name", k)
		}
		out = append(out, fmt.Sprintf(`%s="%s"`, k, escapePromLabelValue(sel[k])))
	}
	return out, nil
}

// buildErrorRatioQuery renders the PromQL for an SLI's error ratio over one
// window. The shapes come from observability-slo → slo.sli_types, expressed as
// the error side (1 - good/valid) because that is what a burn rate divides.
//
// The two types treat a missing numerator deliberately differently:
//
//   - availability ORs the errors numerator with vector(0). A service that has
//     never returned an error may legitimately have no _errors_total series at
//     all, and without the OR the whole ratio evaluates to empty — reporting a
//     perfectly healthy service as NoData forever.
//
//   - latency does NOT. Its numerator is the under-threshold bucket, so
//     defaulting an absent bucket to zero would read as "no request was fast"
//     and fabricate a 100% error ratio — an instant page from a threshold that
//     merely names a bucket the histogram does not publish. Absent means
//     unknown, and unknown must not act.
//
// The denominator is floored rather than guarded, so an idle service reads 0 and
// a service emitting nothing at all still reads empty (no data). The result is
// clamped to [0,1] so a histogram caught mid-scrape cannot manufacture a
// negative or greater-than-one ratio.
func buildErrorRatioQuery(sli governancev1alpha1.SLI, window string) (string, error) {
	if !promIdentifierRE.MatchString(sli.Metric) {
		return "", fmt.Errorf("sli metric %q failed validation; refusing to build query", sli.Metric)
	}
	if !promDurationRE.MatchString(window) {
		return "", fmt.Errorf("burn window %q is not a Prometheus duration", window)
	}
	matchers, err := selectorMatchers(sli.Selector)
	if err != nil {
		return "", err
	}
	sel := ""
	if len(matchers) > 0 {
		sel = "{" + strings.Join(matchers, ",") + "}"
	}

	var ratio string
	switch sli.Type {
	case sliTypeAvailability:
		// The zero-default is scoped to the case it exists for, with absent().
		//
		// A bare `or vector(0)` cannot tell "this service has never errored"
		// from "the errors series does not carry the labels this selector
		// matches" — a differently-instrumented counter, or a metric named
		// _error_total. Both make the numerator empty, and an unscoped default
		// turns the second into a permanent, perfectly healthy zero across every
		// window: budget 100%, severity empty, no condition, no metric, nothing
		// to alert on. An objective that measures nothing and reports success is
		// worse than one that reports nothing.
		//
		// `vector(0) and absent(<m>_errors_total)` yields a sample only when the
		// family does not exist at all — a service that has genuinely never
		// errored — so that case still reads 0, while a selector matching no
		// errors series falls through to an empty result and is reported as
		// NoData. That is the direction the pipeline's own rule demands: absent
		// is not healthy.
		ratio = fmt.Sprintf(
			"(sum(rate(%s_errors_total%s[%s])) or (vector(0) and absent(%s_errors_total))) / clamp_min(sum(rate(%s_requests_total%s[%s])), %s)",
			sli.Metric, sel, window, sli.Metric, sli.Metric, sel, window, sliDenominatorFloor,
		)
	case sliTypeLatency:
		if !promDecimalRE.MatchString(sli.ThresholdSeconds) {
			return "", fmt.Errorf("sli thresholdSeconds %q is required and must be a decimal for a latency SLI", sli.ThresholdSeconds)
		}
		bucketMatchers := make([]string, 0, len(matchers)+1)
		bucketMatchers = append(bucketMatchers, matchers...)
		bucketMatchers = append(bucketMatchers, fmt.Sprintf(`le="%s"`, sli.ThresholdSeconds))
		bucketSel := "{" + strings.Join(bucketMatchers, ",") + "}"
		ratio = fmt.Sprintf(
			"1 - (sum(rate(%s_request_duration_seconds_bucket%s[%s])) / clamp_min(sum(rate(%s_request_duration_seconds_count%s[%s])), %s))",
			sli.Metric, bucketSel, window, sli.Metric, sel, window, sliDenominatorFloor,
		)
	default:
		return "", fmt.Errorf("unknown sli type %q", sli.Type)
	}
	return fmt.Sprintf("clamp_max(clamp_min(%s, 0), 1)", ratio), nil
}

// queryWindows is every distinct window one tick reads: each burn pair's long
// and short window, plus the SLO window for the remaining-budget gauge.
// De-duplicated because the pairs overlap — 6h is the long window of one pair
// and the short window of another — so a naive walk would query it twice.
func queryWindows() []string {
	seen := make(map[string]bool, len(burnWindows)*2+1)
	out := make([]string, 0, len(burnWindows)*2+1)
	add := func(w string) {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	for _, w := range burnWindows {
		add(w.long)
		add(w.short)
	}
	add(sloWindow)
	return out
}

// queryErrorRatios reads the SLI's error ratio for every window this tick needs.
// Windows that returned no data are absent from the map rather than present as
// zero, so the evaluator can refuse to act on a signal it does not have.
func (r *SLOReconciler) queryErrorRatios(ctx context.Context, sli governancev1alpha1.SLI) (map[string]float64, error) {
	prom := r.metricStore(ctx)
	if prom == nil {
		return nil, errAMPNotConfigured
	}
	windows := queryWindows()
	ratios := make(map[string]float64, len(windows))
	for _, w := range windows {
		expr, err := buildErrorRatioQuery(sli, w)
		if err != nil {
			return nil, err
		}
		v, ok, err := prom.QueryScalar(ctx, expr)
		if err != nil {
			return nil, fmt.Errorf("query error ratio over %s: %w", w, err)
		}
		if !ok {
			continue
		}
		ratios[w] = v
	}
	return ratios, nil
}

// burnEvaluation is the tick's verdict across every window pair.
type burnEvaluation struct {
	severity       string
	breachedWindow string
	// pageRatio and ticketRatio are normalized breach ratios: across each pair
	// in the tier, min(long burn, short burn) / factor, taking the largest. One
	// threshold covers pairs with different factors, and the min is what makes
	// it equivalent to "both windows exceed the factor".
	pageRatio      float64
	ticketRatio    float64
	pageHaveData   bool
	ticketHaveData bool
	// anyData is false when not one pair could be evaluated — the signal is
	// missing, which is not the same as healthy.
	anyData bool
}

// evaluateBurn turns per-window error ratios into a breach verdict. Pure over
// its inputs so the whole multi-window rule is unit-testable without AMP.
//
// The comparison is strictly greater-than, matching the operator's own
// PrometheusRule (`ratio > (factor * budget)`): a burn sitting exactly at the
// factor does not breach. Keeping the two evaluators byte-equivalent in that
// detail is the point — the Go loop and the PromQL alert must not disagree about
// the same data.
func evaluateBurn(ratios map[string]float64, budget float64) burnEvaluation {
	var eval burnEvaluation
	if budget <= 0 {
		// A degenerate objective of 1.0 leaves no budget to burn; the CRD
		// pattern already forbids it, so this is a guard, not a path.
		return eval
	}
	// The fastest breaching pair per tier. burnWindows is ordered fast-to-slow
	// within each tier, so the first hit is the shortest long-window that
	// tripped — which is the actionable one: it bounds how quickly the budget is
	// going. Reporting the most-violated pair instead would name a 6h window
	// while a 1h window is also burning, which reads as less urgent than it is.
	breachedBy := map[string]string{}
	for _, w := range burnWindows {
		long, okLong := ratios[w.long]
		short, okShort := ratios[w.short]
		if !okLong || !okShort {
			// Dual-window is the whole method: a long window with no short
			// confirmation is half a check, not a check.
			continue
		}
		burn := long / budget
		if s := short / budget; s < burn {
			burn = s
		}
		ratio := burn / w.factor
		eval.anyData = true

		switch w.severity {
		case severityCritical:
			eval.pageHaveData = true
			if ratio > eval.pageRatio {
				eval.pageRatio = ratio
			}
		case severityWarning:
			eval.ticketHaveData = true
			if ratio > eval.ticketRatio {
				eval.ticketRatio = ratio
			}
		}
		if ratio > 1 {
			if _, seen := breachedBy[w.severity]; !seen {
				breachedBy[w.severity] = w.long
			}
		}
	}

	// Critical wins: a page-tier breach is also a ticket-tier breach, and the
	// higher tier is the one that must route.
	if eval.pageHaveData && eval.pageRatio > 1 {
		eval.severity = severityCritical
		eval.breachedWindow = breachedBy[severityCritical]
		return eval
	}
	if eval.ticketHaveData && eval.ticketRatio > 1 {
		eval.severity = severityWarning
		eval.breachedWindow = breachedBy[severityWarning]
	}
	return eval
}

// errorBudgetRemaining is the unspent fraction of the SLO-window budget, clamped
// to [0,1]. Absent when the SLO window returned no data.
func errorBudgetRemaining(ratios map[string]float64, budget float64) (float64, bool) {
	consumed, ok := ratios[sloWindow]
	if !ok || budget <= 0 {
		return 0, false
	}
	remaining := 1 - (consumed / budget)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	return remaining, true
}

// sloReading is everything one tick decided, ready to be written to status.
type sloReading struct {
	ratios          map[string]float64
	eval            burnEvaluation
	budgetRemaining float64
	haveBudget      bool

	severity       string
	breachedWindow string
	pageRatio      string
	ticketRatio    string

	breachPublished bool

	// Hold decisions. engaging/releasing are transitions this tick; obs is what
	// the AppProject actually showed.
	holdEngaging  bool
	holdReleasing bool
	holdEngaged   bool
	obs           holdObservation
	holdUnobs     bool

	// signalMissing is set when the reconciler could not obtain a reading at all
	// (no AMP client, or every window empty). It is reported as a condition and —
	// critically — it never releases an existing hold.
	signalMissing bool

	platformName string
}

// formatRatio renders a computed ratio for status. Six fractional digits matches
// the precision the budget reconciler writes its decimals at, and is well past
// what a burn-rate comparison is sensitive to.
func formatRatio(f float64) string { return strconv.FormatFloat(f, 'f', 6, 64) }

// tierRatio is the breach ratio of the tier that is currently breaching.
func tierRatio(eval burnEvaluation) float64 {
	if eval.severity == severityCritical {
		return eval.pageRatio
	}
	return eval.ticketRatio
}

// fireBurnRateBreach publishes a BurnRateBreach event to the kill-switch bus.
//
// Unlike the budget breach, the platform action does NOT come back through
// EventBridge: the hold is a Kubernetes write, so the operator that owns
// Kubernetes takes it directly and this event is the routing and audit path —
// the rules page the critical topic, ticket the warning topic, and the bus
// archive retains both. Publishing and acting are therefore independent, and a
// bus that is not configured degrades paging without disabling the control loop.
func (r *SLOReconciler) fireBurnRateBreach(ctx context.Context, sp *governancev1alpha1.SLOPolicy, platformName string, reading sloReading) error {
	if r.EventBridge == nil || r.KillSwitchEventBusName == "" {
		return nil
	}
	detail := map[string]any{
		"platformId":     platformName,
		"namespace":      sp.Namespace,
		"sloPolicy":      sp.Name,
		"sliType":        sp.Spec.SLI.Type,
		"sliMetric":      sp.Spec.SLI.Metric,
		"objective":      sp.Spec.Objective,
		"severity":       reading.eval.severity,
		"breachedWindow": reading.eval.breachedWindow,
		"breachRatio":    formatRatio(tierRatio(reading.eval)),
		"reason":         "slo-burn-rate",
	}
	if reading.haveBudget {
		detail["errorBudgetRemaining"] = formatRatio(reading.budgetRemaining)
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshal burn-rate event: %w", err)
	}
	out, err := r.EventBridge.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []eventbridgetypes.PutEventsRequestEntry{{
			EventBusName: aws.String(r.KillSwitchEventBusName),
			Source:       aws.String(sloEventSource),
			DetailType:   aws.String(sloEventDetailType),
			Detail:       aws.String(string(payload)),
			Time:         aws.Time(time.Now().UTC()),
		}},
	})
	if err != nil {
		return fmt.Errorf("eventbridge PutEvents: %w", err)
	}
	// PutEvents answers 200 even when an entry fails (bad bus name, IAM denial,
	// throttling); the failure is per-entry. Treat any failed entry as a real
	// error so the breach is re-published on the next tick rather than silently
	// dropped — the same trap the budget breach path guards.
	if out.FailedEntryCount > 0 {
		code, msg := "", ""
		if len(out.Entries) > 0 {
			code = aws.ToString(out.Entries[0].ErrorCode)
			msg = aws.ToString(out.Entries[0].ErrorMessage)
		}
		return fmt.Errorf("eventbridge PutEvents partial failure: %d entries failed (%s: %s)", out.FailedEntryCount, code, msg)
	}
	return nil
}

// reconcileSLO is the substantive body: read the signal, decide, verify.
func (r *SLOReconciler) reconcileSLO(ctx context.Context, sp *governancev1alpha1.SLOPolicy) (sloReading, error) {
	platform, err := r.resolveSLOPlatform(ctx, sp)
	if err != nil {
		if errors.Is(err, errPlatformSLONotFound) {
			return sloReading{signalMissing: true}, nil
		}
		return sloReading{}, err
	}
	reading := sloReading{platformName: platform.Name, holdEngaged: sp.Status.HoldEngagedAt != nil}

	objective, err := strconv.ParseFloat(sp.Spec.Objective, 64)
	if err != nil {
		return sloReading{}, fmt.Errorf("parse objective %q: %w", sp.Spec.Objective, err)
	}
	budget := 1 - objective

	ratios, err := r.queryErrorRatios(ctx, sp.Spec.SLI)
	switch {
	case errors.Is(err, errAMPNotConfigured):
		// No AMP on this cluster, or managed-monitoring has not applied yet.
		reading.signalMissing = true
	case err != nil:
		return sloReading{}, err
	default:
		reading.ratios = ratios
		reading.eval = evaluateBurn(ratios, budget)
		reading.budgetRemaining, reading.haveBudget = errorBudgetRemaining(ratios, budget)
		// Only format a tier that actually resolved. pageHaveData/ticketHaveData
		// gated the breach comparison but not the reporting, so a tier whose
		// windows all came back empty was published as "0.000000" — a fabricated
		// healthy reading for a tier that was never evaluated. A single missed
		// scrape empties the 5m window (rate() needs two samples in range) while
		// the 1h/6h/1d/3d windows still resolve, so the 14.4x fast-burn pair
		// drops out of evaluation during exactly the incident that disturbs
		// scraping. Both status fields are omitempty, so an unevaluated tier is
		// absent rather than zero.
		if reading.eval.pageHaveData {
			reading.pageRatio = formatRatio(reading.eval.pageRatio)
		}
		if reading.eval.ticketHaveData {
			reading.ticketRatio = formatRatio(reading.eval.ticketRatio)
		}
		// Every window came back empty: the service is not reporting the series
		// this SLI names. Losing the signal must not look like good news.
		reading.signalMissing = !reading.eval.anyData
	}

	if !reading.signalMissing {
		reading.severity = reading.eval.severity
		reading.breachedWindow = reading.eval.breachedWindow

		// Publish on a new breach episode, and again when the severity changes —
		// an escalation from warning to critical is a page that has not happened
		// yet. A steady-state breach is not re-published: the hold is the action,
		// and it is re-verified every tick.
		if reading.severity != "" && (sp.Status.BreachFiredAt == nil || sp.Status.Severity != reading.severity) {
			if err := r.fireBurnRateBreach(ctx, sp, platform.Name, reading); err != nil {
				return sloReading{}, err
			}
			reading.breachPublished = true
		}

		// The hold is page-tier only, and opt-out via spec.
		want := reading.severity == severityCritical && sp.Spec.OnPageTierBreach == onBreachHoldRollout
		switch {
		case want && sp.Status.HoldEngagedAt == nil:
			reading.holdEngaging = true
			reading.holdEngaged = true
		case !want && sp.Status.HoldEngagedAt != nil && reading.eval.pageHaveData:
			// Only a page tier that actually evaluated clean may release the
			// hold. Without the pageHaveData guard, a page tier that went blind
			// while the ticket tier still resolved would read as "not
			// breaching" and auto-resume the rollout — the same failure the
			// signalMissing path exists to prevent, reached through a partial
			// signal instead of a total one.
			reading.holdReleasing = true
			reading.holdEngaged = false
		}
	}
	// With no signal the hold state is left exactly as it was. Releasing on lost
	// telemetry would auto-resume a rollout during an observability outage, which
	// is precisely when resuming is worst. The window leaves manualSync open, so
	// a human is never locked out.

	if reading.holdEngaged {
		obs, err := r.observeHold(ctx, platform)
		if err != nil {
			return sloReading{}, err
		}
		reading.obs = obs
		reading.holdUnobs = r.holdUnobserved(sp, obs, time.Now())
	}

	// Drop any series this policy owns under a stale platform label first. The
	// platform is a metric label, so a policy repointed at a different Platform
	// would otherwise leave the old platform's gauges pinned at their last value
	// forever — including hold_active at 1 for a tenant nobody is holding.
	forgetSLOPolicySeries(sp.Namespace, sp.Name)

	// Per tier, not per tick: a tier that did not resolve must not export a 0,
	// or a dashboard and an alert both read healthy for a tier nobody measured.
	// Leaving the gauge unset lets it go stale instead, which the staleness
	// alert can see.
	if reading.eval.pageHaveData {
		sloBurnRate.WithLabelValues(sp.Namespace, sp.Name, platform.Name, tierPage).Set(reading.eval.pageRatio)
	}
	if reading.eval.ticketHaveData {
		sloBurnRate.WithLabelValues(sp.Namespace, sp.Name, platform.Name, tierTicket).Set(reading.eval.ticketRatio)
	}
	held := 0.0
	if reading.obs.present {
		held = 1
	}
	sloHoldActive.WithLabelValues(sp.Namespace, sp.Name, platform.Name).Set(held)
	if reading.holdUnobs {
		sloHoldUnobservedTotal.WithLabelValues(sp.Namespace, sp.Name, platform.Name).Inc()
	}

	return reading, nil
}

// applySLOStatus writes the computed reading into status and emits the matching
// conditions.
func (r *SLOReconciler) applySLOStatus(ctx context.Context, sp *governancev1alpha1.SLOPolicy, reading sloReading) error {
	now := metav1.Now()
	previousSeverity := sp.Status.Severity
	sp.Status.LastReconciled = &now
	if !reading.signalMissing {
		// Distinct from LastReconciled on purpose — see the field's doc. This is
		// the one a staleness alert can read, because it stops advancing the
		// moment the objective stops being measured.
		sp.Status.LastEvaluated = &now
	}

	if reading.ratios != nil {
		formatted := make(map[string]string, len(reading.ratios))
		for w, v := range reading.ratios {
			formatted[w] = formatRatio(v)
		}
		sp.Status.ErrorRatios = formatted
	}
	if !reading.signalMissing {
		sp.Status.PageTierBreachRatio = reading.pageRatio
		sp.Status.TicketTierBreachRatio = reading.ticketRatio
		sp.Status.BreachedWindow = reading.breachedWindow
		sp.Status.Severity = reading.severity
		if reading.haveBudget {
			sp.Status.ErrorBudgetRemaining = formatRatio(reading.budgetRemaining)
		}
		if reading.severity == "" {
			// The burn cleared. Drop the latch so a later breach publishes again
			// instead of being swallowed as a duplicate.
			sp.Status.BreachFiredAt = nil
		}
	}
	if reading.breachPublished {
		sp.Status.BreachFiredAt = &now
	}
	if reading.holdEngaging {
		sp.Status.HoldEngagedAt = &now
	}
	if reading.holdReleasing {
		sp.Status.HoldEngagedAt = nil
		sp.Status.HoldObservedAt = nil
	}
	if reading.obs.present {
		sp.Status.HoldObservedAt = &now
	}

	upsertCondition(&sp.Status.Conditions, sloEvaluatedCondition(sp, reading, now))
	upsertCondition(&sp.Status.Conditions, r.burnRateCondition(sp, reading, previousSeverity, now))
	upsertCondition(&sp.Status.Conditions, rolloutHeldCondition(sp, reading, now))

	return r.Status().Update(ctx, sp)
}

// sloEvaluatedCondition reports whether this tick obtained a usable reading.
func sloEvaluatedCondition(sp *governancev1alpha1.SLOPolicy, reading sloReading, now metav1.Time) metav1.Condition {
	cond := metav1.Condition{
		Type:               "SLOEvaluated",
		LastTransitionTime: now,
		ObservedGeneration: sp.Generation,
	}
	switch {
	case reading.platformName == "":
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PlatformNotFound"
		cond.Message = fmt.Sprintf("platformRef %q does not resolve to a Platform in %s", sp.Spec.PlatformRef.Name, sp.Namespace)
	case reading.ratios == nil:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "MetricStoreUnavailable"
		cond.Message = "no Amazon Managed Prometheus endpoint is wired for this cluster, so the objective cannot be evaluated; any engaged rollout hold is deliberately left in place"
	case reading.signalMissing:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NoData"
		cond.Message = fmt.Sprintf("no window returned a reading for %s %q — the series this SLI names are not in the metric store; any engaged rollout hold is deliberately left in place", sp.Spec.SLI.Type, sp.Spec.SLI.Metric)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Evaluated"
		cond.Message = fmt.Sprintf("page-tier breach ratio %s, ticket-tier %s, error budget remaining %s",
			reading.pageRatio, reading.ticketRatio, sp.Status.ErrorBudgetRemaining)
	}
	return cond
}

// burnRateCondition is the breach state itself: True means the error budget is
// burning faster than the standard tolerates.
func (r *SLOReconciler) burnRateCondition(sp *governancev1alpha1.SLOPolicy, reading sloReading, previousSeverity string, now metav1.Time) metav1.Condition {
	cond := metav1.Condition{
		Type:               "BurnRateBreach",
		LastTransitionTime: now,
		ObservedGeneration: sp.Generation,
	}
	switch {
	case reading.signalMissing:
		// Keep the last known verdict visible rather than assert health from an
		// absent signal.
		cond.Status = metav1.ConditionUnknown
		cond.Reason = "SignalUnavailable"
		cond.Message = fmt.Sprintf("the burn rate could not be evaluated this tick; last known severity %q", previousSeverity)
	case reading.severity == "":
		cond.Status = metav1.ConditionFalse
		cond.Reason = "WithinBudget"
		cond.Message = fmt.Sprintf("no burn-rate window pair is over its factor (page tier at %s of the breach threshold, ticket tier at %s)", reading.pageRatio, reading.ticketRatio)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "BurningTooFast"
		published := "not published — no kill-switch bus is configured"
		if reading.breachPublished {
			published = "published to " + r.KillSwitchEventBusName
		} else if sp.Status.BreachFiredAt != nil {
			published = "already published for this episode"
		}
		ratio := reading.ticketRatio
		if reading.severity == severityCritical {
			ratio = reading.pageRatio
		}
		cond.Message = fmt.Sprintf("%s burn: the %s window pair is at %s of its factor; %s",
			reading.severity, reading.breachedWindow, ratio, published)
	}
	return cond
}

// rolloutHeldCondition is tri-state so an operator can tell "held" from "hold
// decided but not landed yet" from "hold is broken" from "not held". Engaging a
// hold is a decision; the AppProject carrying the deny window is the effect, and
// only the effect stops a rollout.
func rolloutHeldCondition(sp *governancev1alpha1.SLOPolicy, reading sloReading, now metav1.Time) metav1.Condition {
	cond := metav1.Condition{
		Type:               "RolloutHeld",
		LastTransitionTime: now,
		ObservedGeneration: sp.Generation,
	}
	switch {
	case !reading.holdEngaged:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NotHeld"
		cond.Message = "no page-tier burn is holding this tenant's rollout"
	case reading.obs.unverifiable:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = "AppProjectAbsent"
		cond.Message = "this tenant has no ArgoCD AppProject on this cluster, so the hold cannot be applied or verified; the breach is still published and paged"
	case reading.obs.present:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "HoldObserved"
		cond.Message = "a deny syncWindow on the tenant's AppProject is holding auto-sync; manual sync remains available for a fix"
	case reading.holdUnobs:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "HoldNotObserved"
		cond.Message = "the hold was engaged but the deny syncWindow has not appeared on the tenant's AppProject within the grace window — the Platform reconciler is not rendering it, and this tenant's rollout is still advancing"
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "AwaitingHold"
		cond.Message = "the hold is engaged; awaiting the deny syncWindow on the tenant's AppProject"
	}
	return cond
}

// applySLOStatusError records SLOEvaluated=False so operators can tell
// "reconciler failing" from "reconciler not running" without reading logs.
// LastReconciled is not bumped — the existing timestamp keeps reflecting the last
// successful tick, which is what a staleness alert wants.
func (r *SLOReconciler) applySLOStatusError(ctx context.Context, sp *governancev1alpha1.SLOPolicy, reason string, cause error) error {
	upsertCondition(&sp.Status.Conditions, metav1.Condition{
		Type:               "SLOEvaluated",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sp.Generation,
	})
	return r.Status().Update(ctx, sp)
}
