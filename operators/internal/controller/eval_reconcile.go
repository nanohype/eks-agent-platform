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
const defaultEvalRunnerNamespace = "eval-runner"

// evalRunnerNamespace returns the per-reconciler namespace where Workflows
// land — RunnerNamespace if set, otherwise the default.
func (r *EvalReconciler) evalRunnerNamespace() string {
	if r.RunnerNamespace != "" {
		return r.RunnerNamespace
	}
	return defaultEvalRunnerNamespace
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
	kind := "Workflow"
	if suite.Spec.Schedule != "" {
		kind = "CronWorkflow"
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: kind})
	obj.SetName(evalWorkflowName(suite))
	obj.SetNamespace(r.evalRunnerNamespace())

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
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
		params := []map[string]any{
			{"name": "platform", "value": platform.Name},
			{"name": "tenant", "value": platform.Spec.Tenant},
			{"name": "fleet", "value": fleet.Name},
			// Pass the bare EvalSuite resource name as a separate
			// parameter so the workflow's writeback step doesn't have
			// to derive it from workflow.name (which is platform-
			// prefixed). Shell parameter expansion on a hyphenated
			// platform name would strip the wrong segment.
			{"name": "suite-name", "value": suite.Name},
			{"name": "pass-threshold", "value": suite.Spec.PassThreshold},
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
			"serviceAccountName":  "eval-runner",
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
	Name           string   `json:"name"`
	Input          string   `json:"input"`
	ExpectContains []string `json:"expectContains"`
	MaxLatencyMs   int32    `json:"maxLatencyMs"`
	MaxCostUsd     string   `json:"maxCostUsd"`
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
			ExpectContains: c.ExpectContains,
			MaxLatencyMs:   c.MaxLatencyMs,
			MaxCostUsd:     c.MaxCostUsd,
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

// reconcileEval is the substantive body. Returns the phase to write
// into status. Errors are real retries; missing-CRD + missing-ref are
// surfaced as Pending so the reconciler doesn't burn on backoff.
func (r *EvalReconciler) reconcileEval(ctx context.Context, suite *governancev1alpha1.EvalSuite) (string, error) {
	platform, fleet, err := r.resolveEvalRefs(ctx, suite)
	if err != nil {
		if errors.Is(err, errEvalPlatformNotFound) || errors.Is(err, errEvalFleetNotFound) {
			return phasePending, nil
		}
		return "", err
	}
	// Don't emit until both Platform AND AgentFleet are Ready — otherwise
	// the Argo job would target a tenant namespace whose IRSA or fleet
	// pods don't exist yet.
	if platform.Status.Phase != phaseReady || fleet.Status.Phase != phaseReady {
		return phasePending, nil
	}
	if err := r.ensureArgoWorkflow(ctx, suite, platform, fleet); err != nil {
		if errors.Is(err, errArgoNotInstalled) {
			return phasePending, nil
		}
		return "", err
	}
	// We don't watch the emitted Workflow's status here — the eval-runner
	// template writes back to suite.status.lastScore + lastRunAt via the
	// in-cluster API at the end of its post-run step. Until that arrives,
	// phase is Provisioning (CronWorkflow installed, no completed run yet)
	// or whatever the previous run left in status.
	if suite.Status.LastRunAt == nil {
		return phaseProvisioning, nil
	}
	return suite.Status.Phase, nil
}

// applyEvalStatus writes the computed phase + condition.
func (r *EvalReconciler) applyEvalStatus(ctx context.Context, suite *governancev1alpha1.EvalSuite, phase string) error {
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
		cond.Message = "waiting on Platform/AgentFleet readiness or Argo CRDs"
	}
	upsertCondition(&suite.Status.Conditions, cond)
	return r.Status().Update(ctx, suite)
}
