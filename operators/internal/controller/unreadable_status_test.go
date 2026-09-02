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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// A condition whose Type names a PROBLEM reads differently from one that names a
// step. "RoutesReconciled=False" says the work is not done, which is true on
// every path that returns early. "ImportedRouteGuardrailUnenforced=False" says
// the routes were walked and none of them is unguarded — and the value that
// produces it, an empty list, is also what an early return leaves behind.
//
// So the problem-named conditions are the ones that can answer a question from
// an evaluation that never ran, and they are the class this enumerates. The
// remedy is in the tree in two spellings: an Unknown status with a Reason naming
// the absent input, and a distinct Reason on a condition whose False was already
// unambiguous.
//
// The mirror-image error costs as much. A value the reconciler read and found
// absent — an optional spec field left unset — is a definite answer, and
// reporting Unknown for it turns the common case into a permanent "did not
// look". Unknown is for a question that could not be asked, not for one whose
// answer is nothing.
//
// Every condition type the package writes is swept from its own source, and the
// sweep resolves a Type written as a literal, as a package constant or as a
// local holding one, because the shipped code uses all three; an unresolved Type
// fails the sweep. A condition in neither table below fails it too.
//
// What the sweep does NOT establish is that any particular un-evaluated path is
// reachable as Unknown: it credits an Unknown to every condition the enclosing
// function builds. That is a tripwire, and the arm-reading tests further down
// are what hold each problem-named condition to its own paths.

// problemNamedConditions are the conditions whose Type names something wrong, so
// False is a claim that it was looked for and not found. Each maps to how it
// avoids making that claim without looking.
var problemNamedConditions = map[string]string{
	"BurnRateBreach":                   "Unknown/SignalUnavailable when the burn rate could not be read",
	"RolloutHeld":                      "Unknown/PlatformNotFound when the platformRef does not resolve, and Unknown/AppProjectAbsent when the hold's effect is unverifiable",
	"ImportedRouteGuardrailUnenforced": "Unknown/RoutesNotEvaluated on every path that returns before the routes are walked",
	"Suspended":                        "Unknown/SuspensionUnreadable when no IAM client is wired, so the role's tag was not read",
	"TenantBudgetExceeded":             "Unknown/SpendIncomplete when a platform's spend leg could not be read; a declared absence of a cap is False/NoAggregateCap, which is an answer",
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
	"VClusterReady":       true,
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

// isConditionLiteral reports whether a composite literal builds a
// metav1.Condition.
func isConditionLiteral(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Condition" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "metav1"
}

// conditionTypeField returns the literal's Type entry, or nil when it sets none
// — a condition built in pieces sets it by assignment instead, and those are
// reached through the enclosing function's other writes.
func conditionTypeField(lit *ast.CompositeLit) *ast.KeyValueExpr {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Type" {
			return kv
		}
	}
	return nil
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
//
// A Type is resolved whether it is written as a literal, as a package constant
// or as a local variable holding one — the shipped code uses all three, and a
// sweep that reads only literals would leave the constant spelling as an escape
// hatch from the classification. A Type it cannot resolve fails the sweep rather
// than being dropped from it.
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
	files := make([]*ast.File, 0, len(names))
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}

	// Package-level string constants, collected first: a Type may be declared in
	// one file and written in another.
	consts := map[string]string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, id := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						consts[id.Name] = v
					}
				}
			}
			return true
		})
	}

	for i, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			// Locals holding a string, for the `condType := "..."` spelling.
			locals := map[string]string{}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				as, ok := m.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for j, lhs := range as.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || j >= len(as.Rhs) {
						continue
					}
					if lit, ok := as.Rhs[j].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							locals[id.Name] = v
						}
					}
				}
				return true
			})

			types := map[string]bool{}
			unknown := false
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "ConditionUnknown" {
					unknown = true
				}
				// Only a metav1.Condition's Type. Plenty of other literals carry
				// a Type field — a seccomp profile, a LimitRange item — and they
				// are not conditions.
				lit, ok := m.(*ast.CompositeLit)
				if !ok || !isConditionLiteral(lit) {
					return true
				}
				kv := conditionTypeField(lit)
				if kv == nil {
					return true
				}
				switch v := kv.Value.(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if s, err := strconv.Unquote(v.Value); err == nil {
							types[s] = true
						}
					}
				case *ast.Ident:
					if s, ok := locals[v.Name]; ok {
						types[s] = true
					} else if s, ok := consts[v.Name]; ok {
						types[s] = true
					} else {
						t.Errorf("%s in %s writes a condition whose Type is %s, which this sweep cannot "+
							"resolve to a string. An unresolved Type is invisible to the classification, "+
							"which is the escape hatch the sweep exists to close", fn.Name.Name, names[i], v.Name)
					}
				default:
					t.Errorf("%s in %s writes a condition whose Type is not a string, a constant or a "+
						"local holding one; teach the sweep its shape rather than leaving it out", fn.Name.Name, names[i])
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
// credits that to every condition the function builds, and it cannot tell one
// un-evaluated case from another. These read the arm itself, for each condition
// whose False is a claim about the world.
func TestTheHoldConditionSeparatesUnevaluatedFromNotHeld(t *testing.T) {
	sp := &governancev1alpha1.SLOPolicy{}
	now := metav1.Now()

	// An unresolved platformRef is the one path that returns before the hold is
	// either applied or read. A missing burn-rate signal is NOT: holdEngaged is
	// seeded from status, and the AppProject is read, on a tick whose metric
	// store is unreachable — so answering Unknown there would deny a hold the
	// reconciler observed.
	got := rolloutHeldCondition(sp, sloReading{platformName: ""}, now)
	if got.Status != metav1.ConditionUnknown || got.Reason != "PlatformNotFound" {
		t.Errorf("a policy whose platformRef does not resolve reports %s/%s; the hold was neither applied "+
			"nor read, and the condition must say so", got.Status, got.Reason)
	}

	got = rolloutHeldCondition(sp, sloReading{platformName: "acme", signalMissing: true, holdEngaged: true}, now)
	if got.Status == metav1.ConditionUnknown {
		t.Errorf("a tick that lost the burn-rate signal reports Unknown for a hold it observed as engaged; "+
			"the hold's state does not come from that signal, and an operator sent here by the alert "+
			"would find the condition denying what fired it (reason %q)", got.Reason)
	}

	got = rolloutHeldCondition(sp, sloReading{platformName: "acme"}, now)
	if got.Status != metav1.ConditionFalse || got.Reason != "NotHeld" {
		t.Errorf("a tick that read no engaged hold reports %s/%s, want False/NotHeld", got.Status, got.Reason)
	}
}

func TestTheSuspendedConditionSeparatesUnreadFromUntagged(t *testing.T) {
	p := &platformv1alpha1.Platform{}

	// The zero result ensureIamRole returns with no IAM client wired has the
	// same shape as a role carrying no tag, and --disable-aws reaches it in
	// shipped configuration.
	got := suspendedCondition(p, false, iamReconcileResult{})
	if got.Status != metav1.ConditionUnknown || got.Reason != "SuspensionUnreadable" {
		t.Errorf("with no IAM client the condition reports %s/%s about a tag nothing read", got.Status, got.Reason)
	}

	got = suspendedCondition(p, true, iamReconcileResult{})
	if got.Status != metav1.ConditionFalse || got.Reason != "NotSuspended" {
		t.Errorf("a role read and found untagged reports %s/%s, want False/NotSuspended; a reconcile that "+
			"looked must give a definite answer", got.Status, got.Reason)
	}

	got = suspendedCondition(p, true, iamReconcileResult{Suspended: true, Reason: "budget"})
	if got.Status != metav1.ConditionTrue || got.Reason != "KillSwitchActive" {
		t.Errorf("a suspended role reports %s/%s, want True/KillSwitchActive", got.Status, got.Reason)
	}
}

func TestTheTenantBudgetConditionSeparatesNoCapFromWithinCap(t *testing.T) {
	// spec.aggregateMonthlyBudgetUsd is optional, and its absence is READ: the
	// reconciler holds the spec and gets a definite answer. That is a
	// nothing-to-report, which this controller reports as False with a Reason of
	// its own — not a could-not-look.
	got := conditionTenantBudget(tenantReading{})
	if got.Status != metav1.ConditionFalse || got.Reason != "NoAggregateCap" {
		t.Errorf("a tenant declaring no cap reports %s/%s; the absence was read, so the answer is "+
			"definite", got.Status, got.Reason)
	}

	// An unread spend leg is the opposite: the sum understates, so "within cap"
	// would be a claim about a total that is not the total.
	got = conditionTenantBudget(tenantReading{capCompared: true})
	if got.Status != metav1.ConditionUnknown || got.Reason != "SpendIncomplete" {
		t.Errorf("a comparison against an understated sum reports %s/%s; at least one platform's spend "+
			"was not readable", got.Status, got.Reason)
	}

	got = conditionTenantBudget(tenantReading{capCompared: true, spendComplete: true})
	if got.Status != metav1.ConditionFalse || got.Reason != "WithinCap" {
		t.Errorf("a complete comparison found under the cap reports %s/%s, want False/WithinCap", got.Status, got.Reason)
	}

	// A partial sum already over the cap is a finding whatever the missing legs
	// would have added.
	got = conditionTenantBudget(tenantReading{capCompared: true, overSpec: true})
	if got.Status != metav1.ConditionTrue {
		t.Errorf("a tenant over its cap reports %s, want True", got.Status)
	}
}

// TestADanglingBudgetRefLeavesTheAggregateIncomplete covers the path between a
// declared absence and an unread value.
//
// spec.budget is required, so a Platform naming a BudgetPolicy that does not
// exist is a dangling reference: its spend is unknown, not zero. The roll-up
// skips the leg, and without recording that it did, the total is short by a
// whole platform while the condition reports a comparison that finished.
func TestEveryLegThatDidNotAnswerLeavesTheComparisonUnfinished(t *testing.T) {
	// One question per leg: did it contribute a number. The reasons a leg can
	// fail to answer are not a list to enumerate — a reference naming nothing,
	// a reference resolving to nothing, a spend that does not parse are the same
	// answer — so each is asserted against the same property rather than against
	// its own case.
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register platform types: %v", err)
	}
	if err := governancev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register governance types: %v", err)
	}
	tenant := &platformv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}

	platform := func(name, budget string) *platformv1alpha1.Platform {
		return &platformv1alpha1.Platform{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ctrlTestNS},
			Spec: platformv1alpha1.PlatformSpec{
				Tenant: "acme",
				Budget: platformv1alpha1.BudgetRef{Name: budget},
			},
		}
	}
	policy := func(name, spend string) *governancev1alpha1.BudgetPolicy {
		bp := &governancev1alpha1.BudgetPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ctrlTestNS},
		}
		bp.Status.CurrentSpendUsd = spend
		return bp
	}

	cases := []struct {
		name    string
		objects []client.Object
		want    bool
	}{
		{"a reference that names nothing", []client.Object{platform("a", "")}, false},
		{"a reference that resolves to nothing", []client.Object{platform("a", "gone")}, false},
		{"a spend that does not parse", []client.Object{platform("a", "b"), policy("b", "not-a-number")}, false},
		{"a spend not yet reported", []client.Object{platform("a", "b"), policy("b", "")}, false},
		{"every leg answered", []client.Object{platform("a", "b"), policy("b", "1.5")}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{tenant}, tc.objects...)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			r := &TenantReconciler{Client: cl, Scheme: scheme}

			reading, err := r.aggregate(context.Background(), tenant)
			if err != nil {
				t.Fatalf("aggregate: %v", err)
			}
			if reading.spendComplete != tc.want {
				t.Errorf("spendComplete = %v, want %v — a leg that did not contribute a number leaves "+
					"the aggregate short by that platform's spend, and the condition would report a "+
					"comparison against a total that is not the total", reading.spendComplete, tc.want)
			}
		})
	}
}
