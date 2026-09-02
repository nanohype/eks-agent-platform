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
	ctlr := dependencyFile(t, dependencySource(t, "sigs.k8s.io/controller-runtime"), "pkg/internal/controller/controller.go")
	for _, token := range []string{"ReconciliationTimeout", "context.WithTimeout"} {
		if !strings.Contains(ctlr, token) {
			t.Errorf("controller-runtime's controller no longer mentions %q; the per-reconcile ceiling this "+
				"operator sets would then bound nothing", token)
		}
	}
}

func TestTheManagerCarriesAReconcileCeiling(t *testing.T) {
	got := controllerOptions().ReconciliationTimeout
	if got <= 0 {
		t.Fatalf("ReconciliationTimeout is %v; zero is controller-runtime's default and disables the "+
			"deadline, so a reconcile stuck on a call none of the per-request bounds cover holds its "+
			"worker until the process restarts", got)
	}

	// A ceiling shorter than a call it contains aborts healthy work. Every
	// timeout this module declares is swept out of its own source, so the
	// comparison covers the ones added after this was written too.
	for name, d := range declaredTimeouts(t) {
		if name == "reconcileTimeout" {
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
				if d, ok := durationValue(vs.Values[i]); ok {
					out[name.Name] = d
				}
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

// durationValue evaluates the `<n> * time.<Unit>` form these constants are
// written in, and reports false for anything else rather than guessing.
func durationValue(e ast.Expr) (time.Duration, bool) {
	bin, ok := e.(*ast.BinaryExpr)
	if !ok || bin.Op != token.MUL {
		return 0, false
	}
	lit, ok := bin.X.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil {
		return 0, false
	}
	sel, ok := bin.Y.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "time" {
		return 0, false
	}
	switch sel.Sel.Name {
	case "Second":
		return time.Duration(n) * time.Second, true
	case "Minute":
		return time.Duration(n) * time.Minute, true
	case "Hour":
		return time.Duration(n) * time.Hour, true
	}
	return 0, false
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
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Controller" {
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
		t.Error("the manager options carry no Controller field, so the per-reconcile ceiling reaches no " +
			"controller and every Reconcile runs with the context controller-runtime hands it unmodified")
	}
}
