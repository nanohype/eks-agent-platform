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
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// evalFinalizer ensures we tear down the Argo CronWorkflow / Workflow
// the reconciler emitted before the EvalSuite is deleted.
const evalFinalizer = "governance.nanohype.dev/eval-finalizer"

// defaultEvalRunnerNamespace is the fallback when the reconciler is built
// without an EvalRunnerNamespace override (envtest / dev paths). Production
// resolves this from SSM via operatorconfig.EvalRunnerNamespace and the
// value flows through EvalReconciler.RunnerNamespace.
const (
	defaultEvalRunnerNamespace      = "eval-runner"
	defaultEvalRunnerServiceAccount = "eval-runner"
)

// errNoReportsBucket is returned when the operator has no eval-reports bucket
// to hand the workflow. Submitting anyway produces a run whose every upload
// targets `s3:///<platform>/…` — the aws CLI rejects that, but only after the
// cases have been executed against a live model, so the tenant pays for the
// inference and gets no report.
var errNoReportsBucket = errors.New(
	"no eval-reports bucket configured: the operator resolves it from " +
		"/eks-agent-platform/<cluster>/eval-runtime/eval_reports_bucket, which the " +
		"eval-runtime terraform component publishes",
)

// evalRunnerNamespace returns the per-reconciler namespace where Workflows
// land — RunnerNamespace if set, otherwise the default.
func (r *EvalReconciler) evalRunnerNamespace() string {
	if r.RunnerNamespace != "" {
		return r.RunnerNamespace
	}
	return defaultEvalRunnerNamespace
}

// evalRunnerServiceAccount returns the ServiceAccount the emitted Workflow
// runs under — RunnerServiceAccount if set, otherwise the default.
func (r *EvalReconciler) evalRunnerServiceAccount() string {
	if r.RunnerServiceAccount != "" {
		return r.RunnerServiceAccount
	}
	return defaultEvalRunnerServiceAccount
}

// argoWorkflowsGV is the GroupVersion the Argo Workflows controller
// owns. Lazy-detected at reconcile time — clusters without Argo
// installed see a NoKindMatch and the reconciler surfaces Pending.
var argoWorkflowsGV = schema.GroupVersion{Group: "argoproj.io", Version: "v1alpha1"}

var (
	errEvalPlatformNotFound = errors.New("eval platformRef not found")
	errEvalFleetNotFound    = errors.New("eval agentFleetRef not found")
	errArgoNotInstalled     = errors.New("argoproj.io Workflows CRDs not installed on this cluster")
)

func (r *EvalReconciler) resolveEvalRefs(ctx context.Context, suite *governancev1alpha1.EvalSuite) (*platformv1alpha1.Platform, *agentsv1alpha1.AgentFleet, error) {
	var p platformv1alpha1.Platform
	pKey := types.NamespacedName{Namespace: suite.Namespace, Name: suite.Spec.PlatformRef.Name}
	if err := r.Get(ctx, pKey, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, errEvalPlatformNotFound
		}
		return nil, nil, fmt.Errorf("get platform %s: %w", pKey, err)
	}
	var fleet agentsv1alpha1.AgentFleet
	fKey := types.NamespacedName{Namespace: suite.Namespace, Name: suite.Spec.AgentFleetRef.Name}
	if err := r.Get(ctx, fKey, &fleet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, errEvalFleetNotFound
		}
		return nil, nil, fmt.Errorf("get agentfleet %s: %w", fKey, err)
	}
	return &p, &fleet, nil
}

// errEvalRouteAmbiguous is a fleet whose agents do not agree on one route.
var errEvalRouteAmbiguous = errors.New("the fleet's agents span more than one model route, so the suite has no single route to exercise")

// errEvalFleetHasNoAgents is a fleet with no agents at all.
var errEvalFleetHasNoAgents = errors.New("the fleet declares no agents, so there is no route to exercise")

// fleetModelRoute is the route an eval run drives: the one the fleet's agents
// are configured with.
//
// Ambiguity is refused rather than resolved. Taking the first agent's route
// would report a score under the suite's name for a route nobody asked about,
// and the reader has no way to tell which one was measured. Same call the
// gateway makes on two ModelGateways over one Platform.
func fleetModelRoute(fleet *agentsv1alpha1.AgentFleet) (string, error) {
	names := make([]string, 0, 1)
	for i := range fleet.Spec.Agents {
		if !slices.Contains(names, fleet.Spec.Agents[i].ModelRoute) {
			names = append(names, fleet.Spec.Agents[i].ModelRoute)
		}
	}
	switch len(names) {
	case 1:
		return names[0], nil
	case 0:
		return "", errEvalFleetHasNoAgents
	default:
		return "", fmt.Errorf("%w: %s", errEvalRouteAmbiguous, strings.Join(names, ", "))
	}
}

// evalWorkflowName is the deterministic name for the Argo object emitted
// for a suite. CronWorkflow when spec.Schedule is set, Workflow
// otherwise. Either way the name is platform-prefixed so two suites
// across two Platforms with the same suite name don't collide.
func evalWorkflowName(suite *governancev1alpha1.EvalSuite) string {
	return suite.Spec.PlatformRef.Name + "-" + suite.Name
}

// ensureArgoWorkflow emits either a CronWorkflow (if Schedule is set) or
// a one-shot Workflow. The pod-spec is intentionally thin — the actual
// container image + script lives in the platform-shared
// `eval-runner` WorkflowTemplate that terraform/components/eval-runtime
// installs; this reconciler just references it via templateRef.
func (r *EvalReconciler) ensureArgoWorkflow(ctx context.Context, suite *governancev1alpha1.EvalSuite, platform *platformv1alpha1.Platform, fleet *agentsv1alpha1.AgentFleet) error {
	// Refuse before submitting rather than after. A run with no bucket still
	// executes every case against a live model and only fails at the upload,
	// so the tenant is billed for the inference and gets nothing back — and
	// the suite reports Running until the workflow times out.
	if r.ReportsBucket == "" {
		return errNoReportsBucket
	}

	// The route contract, read before anything is submitted. Same reason the
	// fleet reads it before writing a Deployment: a run that cannot be given a
	// working base URL should not be submitted at all, because it executes every
	// case against a live model and reports a score for a path that was never
	// reachable.
	routeName, err := fleetModelRoute(fleet)
	if err != nil {
		return err
	}
	routes, err := publishedRoutes(ctx, r.Client, platform)
	if err != nil {
		return err
	}
	route, ok := routes[routeName]
	if !ok {
		return fmt.Errorf("%w: fleet %q runs on route %q; gateway publishes %s",
			errRouteNotPublished, fleet.Name, routeName, strings.Join(routeNames(routes), ", "))
	}

	kind := "Workflow"
	if suite.Spec.Schedule != "" {
		kind = "CronWorkflow"
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: kind})
	obj.SetName(evalWorkflowName(suite))
	obj.SetNamespace(r.evalRunnerNamespace())

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		obj.SetLabels(map[string]string{
			"app.kubernetes.io/managed-by": "eks-agent-platform",
			LabelPlatform:                  platform.Name,
			LabelTenant:                    platform.Spec.Tenant,
			LabelAgentFleet:                fleet.Name,
			LabelEvalSuite:                 suite.Name,
			LabelPassThreshold:             suite.Spec.PassThreshold,
		})

		// Parameters consumed by the platform-shared eval-runner template:
		//   - platform / tenant / fleet → target the right tenant namespace
		//   - cases-source → either inline JSON or s3://… manifest
		//   - pass-threshold → AnalysisTemplate gate value
		// []any, not []map[string]any. This slice is handed to
		// unstructured.SetNestedField, whose deep copy walks only the
		// JSON-native shapes — map[string]interface{} and []interface{}. A
		// []map[string]interface{} panics it ("cannot deep copy"), taking the
		// operator down on every EvalSuite reconcile, and no compiler or vet
		// pass sees it because the argument type is `any`.
		params := []any{
			map[string]any{"name": "platform", "value": platform.Name},
			map[string]any{"name": "tenant", "value": platform.Spec.Tenant},
			map[string]any{"name": "fleet", "value": fleet.Name},
			// The namespace the EvalSuite itself lives in, which is where the
			// writeback step patches its status. `tenant` is a tenant NAME and
			// is not a namespace — patching there addresses a namespace that
			// need not exist, so the run completes and the status it exists to
			// write never lands.
			map[string]any{"name": "suite-namespace", "value": suite.Namespace},
			// The reports bucket the operator read from SSM. A workflow
			// argument overrides the WorkflowTemplate's default, so this is
			// what the run actually uploads to.
			map[string]any{"name": "eval-reports-bucket", "value": r.ReportsBucket},
			// Pass the bare EvalSuite resource name as a separate
			// parameter so the workflow's writeback step doesn't have
			// to derive it from workflow.name (which is platform-
			// prefixed). Shell parameter expansion on a hyphenated
			// platform name would strip the wrong segment.
			map[string]any{"name": "suite-name", "value": suite.Name},
			map[string]any{"name": "pass-threshold", "value": suite.Spec.PassThreshold},
			// The route contract the gateway published, carried whole.
			map[string]any{"name": "model-route-base-url", "value": route.BaseURL},
			map[string]any{"name": "model-route", "value": route.Name},
			map[string]any{"name": "model-route-api", "value": string(route.API)},
		}
		if suite.Spec.CasesFromManifest != "" {
			params = append(params, map[string]any{"name": "cases-manifest", "value": suite.Spec.CasesFromManifest})
		} else {
			inline, err := buildInlineCasesParam(suite.Spec.Cases)
			if err != nil {
				return err
			}
			params = append(params, map[string]any{"name": "cases-inline", "value": inline})
		}

		wfSpec := map[string]any{
			"workflowTemplateRef": map[string]any{"name": "eval-runner"},
			"arguments":           map[string]any{"parameters": params},
			"serviceAccountName":  r.evalRunnerServiceAccount(),
		}

		if kind == "CronWorkflow" {
			spec := map[string]any{
				"schedule":          suite.Spec.Schedule,
				"concurrencyPolicy": "Forbid",
				"workflowSpec":      wfSpec,
			}
			return unstructured.SetNestedField(obj.Object, spec, "spec")
		}
		return unstructured.SetNestedField(obj.Object, wfSpec, "spec")
	})
	if err != nil {
		if isNoKindMatch(err) {
			return errArgoNotInstalled
		}
		return fmt.Errorf("ensure argo %s %s/%s: %w", kind, r.evalRunnerNamespace(), evalWorkflowName(suite), err)
	}
	return nil
}

// inlineCase is the wire shape consumed by the eval-runner script. It
// mirrors governancev1alpha1.EvalCase but with explicit JSON tags so the
// JSON output is the runner's expected schema (jq paths in
// eval-runner reference .name, .input, etc.).
type inlineCase struct {
	Name              string   `json:"name"`
	Input             string   `json:"input"`
	ExpectContains    []string `json:"expectContains"`
	ExpectNotContains []string `json:"expectNotContains"`
	ExpectRefusal     bool     `json:"expectRefusal"`
	MaxLatencyMs      int32    `json:"maxLatencyMs"`
	MaxCostUsd        string   `json:"maxCostUsd"`
}

// buildInlineCasesParam renders the inline cases as a JSON string the
// eval-runner template can pass to its jq pipeline. Uses encoding/json
// so any byte sequence (UTF-8, embedded quotes, control characters) is
// safely escaped — fmt's %q is *Go* quoting, not JSON quoting, and
// produces invalid JSON for control bytes like 0x07 (\a).
func buildInlineCasesParam(cases []governancev1alpha1.EvalCase) (string, error) {
	if len(cases) == 0 {
		return "[]", nil
	}
	wire := make([]inlineCase, len(cases))
	for i, c := range cases {
		wire[i] = inlineCase{
			Name: c.Name, Input: c.Input,
			ExpectContains:    c.ExpectContains,
			ExpectNotContains: c.ExpectNotContains,
			ExpectRefusal:     c.ExpectRefusal,
			MaxLatencyMs:      c.MaxLatencyMs,
			MaxCostUsd:        c.MaxCostUsd,
		}
	}
	b, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("marshal inline eval cases: %w", err)
	}
	return string(b), nil
}

// cleanupArgoWorkflow is the finalizer counterpart: deletes both the
// CronWorkflow and the Workflow variants so a suite that toggled
// Schedule mid-life doesn't leave one of them orphaned.
func (r *EvalReconciler) cleanupArgoWorkflow(ctx context.Context, suite *governancev1alpha1.EvalSuite) error {
	name := evalWorkflowName(suite)
	for _, kind := range []string{"CronWorkflow", "Workflow"} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: kind})
		obj.SetName(name)
		obj.SetNamespace(r.evalRunnerNamespace())
		if err := r.Delete(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) || isNoKindMatch(err) {
				continue
			}
			return fmt.Errorf("delete argo %s %s/%s: %w", kind, r.evalRunnerNamespace(), name, err)
		}
	}
	return nil
}

// reconcileEval is the substantive body. Returns the phase to write into
// status and the reason it is not progressing, if it is not. Errors are real
// retries; missing-CRD + missing-ref are surfaced as Pending so the reconciler
// doesn't burn on backoff.
//
// The reason is returned rather than derived, because every Pending used to
// carry the same sentence — a suite blocked on an unconfigured reports bucket
// read identically to one waiting on a Platform, and the operator was the only
// thing that knew the difference.
func (r *EvalReconciler) reconcileEval(ctx context.Context, suite *governancev1alpha1.EvalSuite) (string, string, error) {
	platform, fleet, err := r.resolveEvalRefs(ctx, suite)
	if err != nil {
		if errors.Is(err, errEvalPlatformNotFound) {
			return phasePending, "platformRef " + suite.Spec.PlatformRef.Name + " not found", nil
		}
		if errors.Is(err, errEvalFleetNotFound) {
			return phasePending, "agentFleetRef " + suite.Spec.AgentFleetRef.Name + " not found", nil
		}
		return "", "", err
	}
	// Don't emit until both Platform AND AgentFleet are Ready — otherwise
	// the Argo job would target a tenant namespace whose identity or fleet
	// pods don't exist yet.
	if platform.Status.Phase != phaseReady || fleet.Status.Phase != phaseReady {
		return phasePending, fmt.Sprintf(
			"waiting on readiness: platform %s is %s, fleet %s is %s",
			platform.Name, platform.Status.Phase, fleet.Name, fleet.Status.Phase), nil
	}
	if err := r.ensureArgoWorkflow(ctx, suite, platform, fleet); err != nil {
		if errors.Is(err, errArgoNotInstalled) {
			return phasePending, errArgoNotInstalled.Error(), nil
		}
		if errors.Is(err, errNoReportsBucket) {
			return phasePending, errNoReportsBucket.Error(), nil
		}
		if errors.Is(err, errGatewayNotFound) || errors.Is(err, errGatewayNotPublished) {
			return phasePending, err.Error(), nil
		}
		if errors.Is(err, errRouteNotPublished) || errors.Is(err, errEvalRouteAmbiguous) ||
			errors.Is(err, errEvalFleetHasNoAgents) || errors.Is(err, errGatewayAmbiguous) {
			return phaseFailed, err.Error(), nil
		}
		return "", "", err
	}
	// We don't watch the emitted Workflow's status here — the eval-runner
	// template writes back to suite.status.lastScore + lastRunAt via the
	// in-cluster API at the end of its post-run step. Until that arrives,
	// phase is Provisioning (CronWorkflow installed, no completed run yet)
	// or whatever the previous run left in status.
	if suite.Status.LastRunAt == nil {
		return phaseProvisioning, "", nil
	}
	return suite.Status.Phase, "", nil
}

// applyEvalStatus writes the computed phase + condition.
func (r *EvalReconciler) applyEvalStatus(ctx context.Context, suite *governancev1alpha1.EvalSuite, phase, reason string) error {
	suite.Status.Phase = phase
	cond := metav1.Condition{
		Type:               "EvalReconciled",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("argo workflow %s/%s in sync", r.evalRunnerNamespace(), evalWorkflowName(suite)),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: suite.Generation,
	}
	// Healthy phases written by either us (Provisioning while waiting on a
	// first run) or the eval-runner template (Ready / Passed once a run
	// completes successfully). Anything else surfaces a False condition
	// so dashboards can see the suite is degraded.
	switch phase {
	case phaseProvisioning, phaseReady, "Passed":
		// healthy — condition stays True
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = phase
		cond.Message = reason
		if cond.Message == "" {
			cond.Message = "not progressing, and the reconciler reported no reason"
		}
	}
	// Surface the last observed mean score as an operator-emitted gauge (the
	// eval-runner writes it back into status; we mirror it as a real series so
	// the eval-quality dashboard has a first-class metric, not only a KSM
	// projection). Skip when the suite has not completed a run yet.
	if v, ok := parseDecimal(suite.Status.LastScore); ok {
		f, _ := v.Float64()
		evalSuiteScore.WithLabelValues(suite.Namespace, suite.Spec.PlatformRef.Name, suite.Name).Set(f)
	}
	upsertCondition(&suite.Status.Conditions, cond)
	return r.Status().Update(ctx, suite)
}
