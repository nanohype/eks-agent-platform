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

	got := controllerOptions(time.Hour).ReconciliationTimeout
	if got <= 0 {
		t.Fatalf("ReconciliationTimeout is %v; zero is controller-runtime's default and disables the "+
			"deadline, so a reconcile stuck on a call none of the per-request bounds cover holds its "+
			"worker until the process restarts", got)
	}

	// And the declared bounds, swept from the module's own source. A constant
	// this cannot evaluate is a failure rather than a silent omission: the
	// comparison is only worth what it covers.
	for name, d := range declaredTimeouts(t) {
		if strings.HasPrefix(name, "reconcile") {
			continue
		}
		if d >= got {
			t.Errorf("%s is %v and the reconcile ceiling is %v; a reconcile making that call would be "+
				"cancelled before the call it is waiting on could return", name, d, got)
		}
	}
}

// declaredTimeouts returns every duration constant in the module whose name ends
// in Timeout, keyed by name.
func declaredTimeouts(t *testing.T) map[string]time.Duration {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	out := map[string]time.Duration{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
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
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if !strings.HasSuffix(name.Name, "Timeout") || i >= len(vs.Values) {
					continue
				}
				d, ok := durationValue(vs.Values[i])
				if !ok {
					// Silently dropping the ones it cannot read is how a sweep
					// comes to cover less than it claims — and the shapes it
					// cannot read are the unusual ones, which is where a bound
					// longer than the ceiling is most likely to be written.
					t.Errorf("%s in %s is a timeout this sweep cannot evaluate, so it is compared against "+
						"nothing. Teach durationValue its shape rather than leaving it out", name.Name, path)
					continue
				}
				out[name.Name] = d
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the sweep found no timeout constant in the module, so the comparison below covers nothing")
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
