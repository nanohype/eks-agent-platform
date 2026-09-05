/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nanohype/eks-agent-platform/operators/internal/controller"
)

// The host API-server client is the one remote this operator talks to without a
// request timeout, and that is a decision rather than an omission. These read
// the decision's premises out of the dependencies that hold them, so the
// reasoning in main.go is checkable instead of remembered.
//
// The premises are three. A rest.Config timeout reaches every request the
// client makes, and a watch is one. A watch is already bounded, by the
// reflector, without this operator setting anything. And controller-runtime
// takes a per-reconcile deadline, which is the bound that fits where a request
// timeout does not.
//
// A dependency bump that moves any of them should be read, not absorbed, which
// is what makes this a test rather than a comment.

func TestAWatchIsBoundedWithoutTheClientConfig(t *testing.T) {
	clientGo := dependencySource(t, "k8s.io/client-go")

	reflector := dependencyFile(t, clientGo, "tools/cache/reflector.go")
	for _, req := range []struct{ token, why string }{
		{"minWatchTimeout", "the reflector's own floor on how long a watch may stay open"},
		{"TimeoutSeconds:", "the field that carries that floor to the apiserver on each watch request"},
	} {
		if !strings.Contains(reflector, req.token) {
			t.Errorf("client-go's reflector no longer mentions %q — %s. The claim that a watch is bounded "+
				"without this operator setting anything rests on it; re-read the reflector before trusting "+
				"the reasoning in main.go", req.token, req.why)
		}
	}

	// The other half: a rest.Config timeout is not watch-aware. It becomes the
	// HTTP client's timeout, and every request reads it back — which is why it
	// is the wrong instrument here and the right one on a client that opens no
	// watch.
	if cfg := dependencyFile(t, clientGo, "rest/config.go"); !strings.Contains(cfg, "Timeout:                   config.Timeout") {
		t.Error("client-go no longer copies rest.Config.Timeout onto the HTTP client it builds; the reason " +
			"the host client sets no request timeout depends on it reaching watches too")
	}
	if req := dependencyFile(t, clientGo, "rest/request.go"); !strings.Contains(req, "timeout = c.Client.Timeout") {
		t.Error("client-go no longer reads the HTTP client's timeout onto each request")
	}

	// And the bound that does fit: controller-runtime deadlines the context it
	// hands Reconcile, when asked to.
	// The call, not the names. "ReconciliationTimeout" appears on struct fields
	// and in a field copy whether or not a deadline is ever applied, and
	// "context.WithTimeout" matches WithTimeoutCause as a substring while a
	// literal context.WithTimeout also sits on the unrelated cache-sync path. A
	// check on either survives the guardrail's removal.
	ctlr := dependencyFile(t, dependencySource(t, "sigs.k8s.io/controller-runtime"), "pkg/internal/controller/controller.go")
	const applied = "context.WithTimeoutCause(ctx, c.ReconciliationTimeout"
	if !strings.Contains(ctlr, applied) {
		t.Errorf("controller-runtime no longer applies the reconcile deadline as %q; the ceiling this "+
			"operator sets would then bound nothing, and every reconcile would run with the context "+
			"controller-runtime hands it unmodified", applied)
	}
}

func TestTheManagerCarriesAReconcileCeiling(t *testing.T) {
	// The bound this ceiling has to clear is not declared anywhere as a
	// constant: the budget reconciler computes it from its requeue interval, so
	// it moves with a flag. A sweep of declared constants cannot see it, which
	// is why it is compared directly, across the interval's range rather than at
	// one value.
	for _, interval := range []time.Duration{
		0,                // unset: the reconciler falls back to its own default
		time.Minute,      // shorter than the poll's floor
		30 * time.Minute, // shorter than the shipped default
		time.Hour,        // the shipped default
		6 * time.Hour,    // an operator that widens it
		24 * time.Hour,
	} {
		ceiling := reconcileCeiling(interval)
		inner := controller.BudgetQueryTimeout(interval)
		if ceiling <= inner {
			t.Errorf("with --budget-requeue-interval=%v the Athena poll bounds itself at %v and the "+
				"reconcile ceiling is %v; the cost query is cancelled every tick, so the spend is never "+
				"read and the cap is never enforced while the scan is billed each time", interval, inner, ceiling)
		}
	}

	// The declared bounds are compared against the SMALLEST ceiling any
	// configuration produces, not against one chosen interval. A constant that
	// clears the ceiling at the shipped default and not at a shorter one is a
	// reconcile cancelled on a call it is waiting for, on a cluster that only
	// tightened a flag.
	floor := reconcileCeiling(0)
	for _, interval := range []time.Duration{0, time.Nanosecond, time.Minute, time.Hour, 24 * time.Hour} {
		if c := reconcileCeiling(interval); c < floor {
			floor = c
		}
	}
	if floor <= 0 {
		t.Fatalf("the smallest reconcile ceiling any configuration produces is %v; zero is "+
			"controller-runtime's default and disables the deadline", floor)
	}

	// Every duration the module declares, whatever it is named. A comparison set
	// chosen by a naming convention is a list, and a wait named anything else
	// escapes it — so a duration at or above the floor is either a bound a
	// reconcile can wait on, which is a failure, or it is recorded below with
	// why a reconcile never waits on it.
	for name, d := range declaredTimeouts(t) {
		if strings.HasSuffix(name, ":reconcileFloor") {
			continue // the ceiling's own floor, which is what everything else is measured against
		}
		if notAReconcileWait[name] != "" {
			continue
		}
		if d >= floor {
			t.Errorf("%s is %v and the smallest reconcile ceiling any flag configuration produces is %v; "+
				"a reconcile making that call would be cancelled before the call it is waiting on could "+
				"return", name, d, floor)
		}
	}
}

// TestEveryDurationFlagIsWeighedAgainstTheCeiling closes the shape the ceiling
// gate cannot otherwise see: a bound computed from a flag is invisible to a
// sweep of declared constants, and the next such flag is invisible to a
// comparison written against the one that exists today.
//
// Every duration flag the binary declares is either read by reconcileCeiling —
// so widening it widens the ceiling — or recorded here with why it cannot
// lengthen a reconcile.
func TestEveryDurationFlagIsWeighedAgainstTheCeiling(t *testing.T) {
	// Flags that bound nothing a reconcile waits on. A requeue interval is how
	// often a reconcile STARTS; it lengthens a reconcile only where something
	// derives a call bound from it, which is what the budget reconciler does and
	// these do not.
	cannotLengthenAReconcile := map[string]string{
		"slo-requeue-interval":    "how often the SLO reconciler ticks; no call bound is derived from it",
		"tenant-requeue-interval": "how often the Tenant reconciler re-aggregates; no call bound is derived from it",
		"config-poll-interval": "how often the substrate is re-read while it is incomplete. It runs in a " +
			"manager runnable that finishes before any reconciler is registered, so no reconcile is ever " +
			"inside it and there is no ceiling for it to exceed",
		"config-absent-report-after": "when a still-absent substrate is reported at error level. It " +
			"bounds nothing at all — no call, no wait a reconcile makes — and changes only the level a " +
			"log line is written at",
	}
	readByTheCeiling := map[string]bool{"budget-requeue-interval": true}

	for _, name := range durationFlagNames(t) {
		if readByTheCeiling[name] || cannotLengthenAReconcile[name] != "" {
			continue
		}
		t.Errorf("--%s is a duration flag the reconcile ceiling neither reads nor accounts for. If any "+
			"call bound is derived from it, widening the flag widens a wait the ceiling would then cut; "+
			"pass it to reconcileCeiling, or record why it cannot lengthen a reconcile", name)
	}
	for name := range readByTheCeiling {
		if !slices.Contains(durationFlagNames(t), name) {
			t.Errorf("the ceiling reads --%s, which the binary no longer declares", name)
		}
	}
	for name := range cannotLengthenAReconcile {
		if !slices.Contains(durationFlagNames(t), name) {
			t.Errorf("a flag record names --%s, which the binary no longer declares; delete the entry", name)
		}
	}
}

// TestNoControllerOverridesTheCeiling reads the setup path rather than the
// option: controller-runtime lets a controller carry its own
// ReconciliationTimeout, which replaces the manager's for that controller alone
// and is read by nothing here.
func TestNoControllerOverridesTheCeiling(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "cmd/main.go" {
			return nil // where the manager-wide ceiling is set
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), "ReconciliationTimeout") {
			t.Errorf("%s sets a ReconciliationTimeout of its own. A per-controller value replaces the "+
				"manager's for that controller, so the ceiling every other gate here reasons about "+
				"stops applying to it", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
}

// durationFlagNames returns every duration flag package main declares, across
// every file of the package rather than one filename.
func durationFlagNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	var out []string
	visit := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		// Both stdlib spellings: DurationVar binds into a variable, Duration
		// returns a pointer. A sweep that knows one of them is a sweep over a
		// spelling rather than over the flags the binary declares.
		var nameArg ast.Expr
		switch sel.Sel.Name {
		case "DurationVar":
			nameArg = call.Args[1]
		case "Duration":
			nameArg = call.Args[0]
		default:
			return true
		}
		if lit, ok := nameArg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, v)
			}
		}
		return true
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(f, visit)
	}
	if len(out) == 0 {
		t.Fatal("package main declares no duration flag, which cannot be true; the sweep is matching nothing")
	}
	sort.Strings(out)
	return out
}

// notAReconcileWait records durations the module declares that no reconcile
// waits on, with the reason. An entry is a claim about what the value is for.
var notAReconcileWait = map[string]string{
	"internal/controller/budget_reconcile.go:killSwitchMaxRefireBackoff": "the ceiling on the interval BETWEEN re-publishes of an unrouted breach, " +
		"compared against wall-clock time across reconciles; no reconcile waits on it",
}

// isDurationExpr reports whether an expression is a duration the gate has to be
// able to evaluate: it names a time unit, or it is built from an identifier the
// sweep has already resolved as one. The second half is what stops a duration
// composed of other durations from being dropped without a word — the shape the
// module uses whenever a window is expressed against an interval.
//
// A constant whose value is an int or a string is not a duration however it is
// named; several in this module carry TTL or Duration in the name and hold a
// count of seconds.
func isDurationExpr(e ast.Expr, seen map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "time" {
				found = true
			}
		case *ast.Ident:
			if seen[v.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// declaredTimeouts returns the durations this gate can weigh, and its scope is
// the honest half of the claim above.
//
// HOLDS: every duration DECLARED in this module — any package-level constant or
// variable, whatever it is named — whose value is built from literals, time
// units, or other declared durations. One that cannot be evaluated fails the
// gate rather than being dropped from it.
//
// DOES NOT HOLD: a duration this module never declares. Three shapes are known
// to be outside it and are not claimed — one built inline at a call site, one
// read from the environment at startup, and one whose value comes from a CRD
// field default. Each would be a wait no gate here compares, and closing them
// needs a reader of a different kind: a call-site walk, an env inventory, and
// the CRD schemas respectively.
//
// declaredTimeouts returns every duration constant in the module whose name ends
// in Timeout, keyed by name.
func declaredTimeouts(t *testing.T) map[string]time.Duration {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	out := map[string]time.Duration{}
	seen := map[string]bool{}

	// Two passes: the first resolves the durations written against a time unit,
	// the second the ones written against those. Keys carry the file, so two
	// packages declaring one identifier cannot erase each other's value or
	// inherit each other's excuse.
	for pass := 0; pass < 2; pass++ {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				if d.Name() == "bin" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", rel, perr)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				vs, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if v, ok := durationValue(vs.Values[i]); ok {
						out[rel+":"+name.Name] = v
						seen[name.Name] = true
						continue
					}
					if pass == 0 || !isDurationExpr(vs.Values[i], seen) {
						continue
					}
					t.Errorf("%s in %s is a duration this sweep cannot evaluate, so it is compared "+
						"against nothing. Teach durationValue its shape rather than leaving it out",
						name.Name, rel)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk the module: %v", err)
		}
	}
	if len(out) == 0 {
		t.Fatal("the sweep found no duration constant in the module, so the comparison covers nothing")
	}
	return out
}

// durationValue evaluates the duration forms these constants are written in —
// `<n> * time.<Unit>`, the same reversed, and a bare `time.<Unit>` — and reports
// false for anything else rather than guessing. A false is a gate failure at the
// caller, not an omission.
func durationValue(e ast.Expr) (time.Duration, bool) {
	switch v := e.(type) {
	case *ast.SelectorExpr: // time.Hour
		return timeUnit(v)
	case *ast.BinaryExpr:
		if v.Op != token.MUL {
			return 0, false
		}
		// Either operand may carry the count: 30 * time.Second and
		// time.Minute * 20 are the same duration written two ways.
		if n, ok := intLiteral(v.X); ok {
			if unit, ok := durationValue(v.Y); ok {
				return time.Duration(n) * unit, true
			}
		}
		if n, ok := intLiteral(v.Y); ok {
			if unit, ok := durationValue(v.X); ok {
				return time.Duration(n) * unit, true
			}
		}
	}
	return 0, false
}

func timeUnit(sel *ast.SelectorExpr) (time.Duration, bool) {
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "time" {
		return 0, false
	}
	switch sel.Sel.Name {
	case "Millisecond":
		return time.Millisecond, true
	case "Second":
		return time.Second, true
	case "Minute":
		return time.Minute, true
	case "Hour":
		return time.Hour, true
	}
	return 0, false
}

func intLiteral(e ast.Expr) (int, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	return n, err == nil
}

// dependencySource returns a module's on-disk source directory, so a claim about
// its behaviour can be read instead of recalled.
func dependencySource(t *testing.T, module string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", module).Output()
	if err != nil {
		t.Fatalf("locate %s: %v", module, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("%s resolves to no directory; its source cannot be read", module)
	}
	return dir
}

func dependencyFile(t *testing.T, dir, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s from %s: %v", rel, dir, err)
	}
	return string(body)
}

// TestTheManagerIsGivenTheCeiling reads the options main builds, not the helper
// that returns them. A helper returning a correct value that nothing passes to
// the manager leaves every reconcile unbounded and every other test here green.
func TestTheManagerIsGivenTheCeiling(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var found, carriesController bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewManager" {
			return true
		}
		found = true
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Controller" {
					continue
				}
				// The key alone says nothing. `Controller: crconfig.Controller{}`
				// is present and zero, and zero is the value that disables the
				// deadline — the state this test exists to catch, wearing the
				// shape it checks for. So the VALUE has to be the call that
				// computes the ceiling.
				call, ok := kv.Value.(*ast.CallExpr)
				if !ok {
					continue
				}
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "controllerOptions" {
					carriesController = true
				}
			}
		}
		return true
	})

	if !found {
		t.Fatal("main.go builds no manager; this test is reading the wrong file")
	}
	if !carriesController {
		t.Error("the manager options do not pass controllerOptions() as their Controller field. A field " +
			"that is absent, or present and zero, leaves the ceiling reaching no controller — and zero " +
			"is controller-runtime's default, so the two are the same outcome")
	}
}
