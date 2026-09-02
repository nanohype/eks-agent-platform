/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
)

// A condition whose Type names a PROBLEM reads differently from one that names a
// step. "RoutesReconciled=False" says the work is not done, which is true on
// every path that returns early. "ImportedRouteGuardrailUnenforced=False" says
// the routes were walked and none of them is unguarded — and the value that
// produces it, an empty list, is also what an early return leaves behind.
//
// So the problem-named conditions are the ones that can answer a question from
// an evaluation that never ran, and they are the class this enumerates. The
// remedy is already in the tree twice, in two spellings: an Unknown status with
// a Reason naming the absent input, and a distinct Reason on a condition whose
// False was already unambiguous.
//
// The table below is not the list of conditions someone remembered. Every
// condition type the package writes is swept out of the source, and one absent
// from both tables fails.

// problemNamedConditions are the conditions whose Type names something wrong, so
// False is a claim that it was looked for and not found. Each maps to how it
// avoids making that claim without looking.
var problemNamedConditions = map[string]string{
	"BurnRateBreach":                   "Unknown/SignalUnavailable when the burn rate could not be read",
	"RolloutHeld":                      "Unknown/SignalUnavailable on a tick with no reading, and Unknown/AppProjectAbsent when the hold's effect is unverifiable",
	"ImportedRouteGuardrailUnenforced": "Unknown/RoutesNotEvaluated on every path that returns before the routes are walked",
	"Suspended":                        "Unknown/SuspensionUnreadable when no IAM client is wired, so the role's tag was not read",
	"TenantBudgetExceeded":             "Unknown/NoAggregateCap when the tenant declares no cap to be within",
	"KillSwitchUnrouted":               alwaysEvaluated,
}

// alwaysEvaluated marks a condition that cannot be written without its
// evaluation having run, so it needs no third status. The claim is checked: a
// condition marked this way must NOT carry an Unknown path, because one would
// mean the writer does have an un-evaluated case to describe.
const alwaysEvaluated = "written only on a path where the reading exists"

// stateNamedConditions name a step or a state rather than a problem. False means
// "not done", which is what an early return should say, so there is nothing for
// a third status to disambiguate.
var stateNamedConditions = map[string]bool{
	"AgentsReconciled":    true,
	"Aggregated":          true,
	"BudgetReconciled":    true,
	"CapabilitiesGranted": true,
	"EvalReconciled":      true,
	"ModelAccessScoped":   true,
	"NamespaceReady":      true,
	"RoutesReconciled":    true,
	"SessionReconciled":   true,
	"SLOEvaluated":        true,
	"WorkersReconciled":   true,
}

func TestEveryConditionIsClassified(t *testing.T) {
	written := conditionTypesWritten(t)
	for name := range written {
		_, problem := problemNamedConditions[name]
		if !problem && !stateNamedConditions[name] {
			t.Errorf("the condition %q is in neither table. Decide what its False SAYS: if the Type names a "+
				"problem, False is a claim it was looked for and not found, and every path that writes it "+
				"without looking answers a question the operator never asked", name)
		}
	}
	for name := range problemNamedConditions {
		if !written[name] {
			t.Errorf("problemNamedConditions names %q, which this package no longer writes; delete the entry", name)
		}
	}
	for name := range stateNamedConditions {
		if !written[name] {
			t.Errorf("stateNamedConditions names %q, which this package no longer writes; delete the entry", name)
		}
	}
}

func TestEveryProblemNamedConditionCanSayItDidNotLook(t *testing.T) {
	writers := conditionWriters(t)

	for name, remedy := range problemNamedConditions {
		fns := writers[name]
		if len(fns) == 0 {
			t.Errorf("no function in this package constructs %q; the sweep cannot check its remedy", name)
			continue
		}
		hasUnknown := false
		for _, fn := range fns {
			if fn.unknown {
				hasUnknown = true
			}
		}
		switch {
		case remedy == alwaysEvaluated && hasUnknown:
			t.Errorf("%q is recorded as written only where the reading exists, and its writer carries an "+
				"Unknown path — so there IS an un-evaluated case, and the record is wrong about which "+
				"one it is", name)
		case remedy != alwaysEvaluated && !hasUnknown:
			t.Errorf("%q is recorded as remedied by %q, and no function constructing it mentions "+
				"ConditionUnknown. Its False then reads as a finding on every path, including the ones "+
				"that return before the evaluation", name, remedy)
		}
	}
}

// TestTheGuardrailConditionSeparatesUnevaluatedFromClean reads the object the
// reconciler writes, for each of the three sentences the condition can carry.
func TestTheGuardrailConditionSeparatesUnevaluatedFromClean(t *testing.T) {
	cases := []struct {
		name   string
		result gatewayReconcileResult
		status metav1.ConditionStatus
		reason string
	}{
		{
			// Every not-ready return leaves the unenforced list nil: no Platform,
			// a Platform not yet Ready, the Gateway-API CRDs absent.
			name:   "a pass that never reached the routes says so",
			result: gatewayReconcileResult{phase: phasePending},
			status: metav1.ConditionUnknown,
			reason: "RoutesNotEvaluated",
		},
		{
			name:   "routes walked and all guarded is a finding, not a default",
			result: gatewayReconcileResult{phase: phaseReady},
			status: metav1.ConditionFalse,
			reason: "NotApplicable",
		},
		{
			name:   "an unguarded imported route is named",
			result: gatewayReconcileResult{phase: phaseReady, unenforcedGuardrail: []string{"imported-a"}},
			status: metav1.ConditionTrue,
			reason: "InlineGuardrailNotApplicable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mgwScheme(t)
			mg := mixedRouteGateway()
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(mg).WithStatusSubresource(mg).Build()
			r := &ModelGatewayReconciler{Client: cl, Scheme: s}

			if err := r.modelGatewayApplyStatus(context.Background(), mg, tc.result); err != nil {
				t.Fatalf("modelGatewayApplyStatus: %v", err)
			}
			var got *metav1.Condition
			for i := range mg.Status.Conditions {
				if mg.Status.Conditions[i].Type == "ImportedRouteGuardrailUnenforced" {
					got = &mg.Status.Conditions[i]
				}
			}
			if got == nil {
				t.Fatal("no ImportedRouteGuardrailUnenforced condition was written")
			}
			if got.Status != tc.status {
				t.Errorf("status = %s, want %s (reason %q, message %q)", got.Status, tc.status, got.Reason, got.Message)
			}
			if got.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.reason)
			}
		})
	}
}

// conditionWriter records what one function's body does with a condition type.
type conditionWriter struct {
	name    string
	unknown bool
}

// conditionTypesWritten returns every condition Type this package's shipped
// source constructs.
func conditionTypesWritten(t *testing.T) map[string]bool {
	out := map[string]bool{}
	for name := range conditionWriters(t) {
		out[name] = true
	}
	if len(out) == 0 {
		t.Fatal("the sweep found no condition in this package, so both tables would pass vacuously")
	}
	return out
}

// conditionWriters maps a condition Type to the functions that construct it,
// recording whether each can produce an Unknown status.
func conditionWriters(t *testing.T) map[string][]conditionWriter {
	t.Helper()
	out := map[string][]conditionWriter{}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	fset := token.NewFileSet()
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			types := map[string]bool{}
			unknown := false
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "ConditionUnknown" {
					unknown = true
				}
				kv, ok := m.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Type" {
					return true
				}
				if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						types[v] = true
					}
				}
				return true
			})
			for ct := range types {
				out[ct] = append(out[ct], conditionWriter{name: fn.Name.Name, unknown: unknown})
			}
			return true
		})
	}
	return out
}

// The structural check above asks whether a writer CAN say "did not look". It
// cannot tell one un-evaluated case from another, and a condition with more than
// one keeps passing when a single arm is removed. These read the arm itself.
func TestTheHoldConditionSeparatesUnevaluatedFromNotHeld(t *testing.T) {
	sp := &governancev1alpha1.SLOPolicy{}
	now := metav1.Now()

	// A tick that never resolved the Platform never seeded holdEngaged, so
	// without its own arm this falls into NotHeld and reports a hold released
	// that status.holdEngagedAt still records as engaged.
	got := rolloutHeldCondition(sp, sloReading{signalMissing: true}, now)
	if got.Status != metav1.ConditionUnknown || got.Reason != "SignalUnavailable" {
		t.Errorf("a tick with no reading reports %s/%s; it did not evaluate the hold and must say so",
			got.Status, got.Reason)
	}

	got = rolloutHeldCondition(sp, sloReading{holdEngaged: false}, now)
	if got.Status != metav1.ConditionFalse || got.Reason != "NotHeld" {
		t.Errorf("a tick that read no engaged hold reports %s/%s, want False/NotHeld", got.Status, got.Reason)
	}
}

func TestTheTenantBudgetConditionSeparatesNoCapFromWithinCap(t *testing.T) {
	// spec.aggregateMonthlyBudgetUsd is optional, so "no cap declared" is the
	// common case, and a tenant with nothing to be within must not report that
	// it is within it.
	got := conditionTenantBudget(tenantReading{})
	if got.Status != metav1.ConditionUnknown || got.Reason != "NoAggregateCap" {
		t.Errorf("a tenant declaring no cap reports %s/%s; there was no comparison to report",
			got.Status, got.Reason)
	}

	got = conditionTenantBudget(tenantReading{capCompared: true})
	if got.Status != metav1.ConditionFalse || got.Reason != "WithinCap" {
		t.Errorf("a tenant compared against its cap and found under it reports %s/%s, want False/WithinCap",
			got.Status, got.Reason)
	}

	got = conditionTenantBudget(tenantReading{capCompared: true, overSpec: true})
	if got.Status != metav1.ConditionTrue {
		t.Errorf("a tenant over its cap reports %s, want True", got.Status)
	}
}
