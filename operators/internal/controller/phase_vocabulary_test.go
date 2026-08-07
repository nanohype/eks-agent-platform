/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// A status field's doc comment is the only place a reader learns which phases a
// resource can report. Nothing compiles that sentence, so it drifts from the
// code silently and in the direction that flatters: DatastoreStatus.Phase
// advertised "Drifted" for as long as the type existed, there was no
// phaseDrifted constant anywhere, and no code path could produce one — the
// operator holds no client for RDS, DynamoDB, ElastiCache, SQS or MSK, so it
// observes no datastore state to derive a phase from. Alongside it sat a
// `Drift []string` status field whose doc read "Empty when in sync", written by
// nothing, so its permanent emptiness read as a positive assertion of health.
//
// These two tests pin the vocabulary to the code. They are deliberately narrow:
// they do not try to prove a phase is reachable, only that it is NAMED by the
// operator and that the one status whose phase is copied cannot claim more than
// its source.

// The one machine-readable form. Prose may say anything above it, but every
// status type that reports a phase must carry a line of exactly this shape, and
// the parse below refuses to run if it finds none.
var phaseDocRE = regexp.MustCompile(`(?m)^\s*//\s*Phase:\s*([^.]*)\.`)

// declaredPhaseConstants reads the phase constants the controller package
// defines, which is the operator's whole phase vocabulary.
func declaredPhaseConstants(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "phases.go", nil, 0)
	if err != nil {
		t.Fatalf("parse phases.go: %v", err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, v := range vs.Values {
			if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				out[strings.Trim(lit.Value, `"`)] = true
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("found no phase constants in phases.go — this test would pass vacuously")
	}
	return out
}

// advertisedPhases pulls the phase names out of every `// Phase: A, B, C.` or
// `// Phase mirrors ... (A, B, C)` doc comment in the API package.
func advertisedPhases(t *testing.T) map[string][]string {
	t.Helper()
	dir := filepath.Join("..", "..", "api", "platform", "v1alpha1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read api dir: %v", err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_types.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range phaseDocRE.FindAllStringSubmatch(string(b), -1) {
			list := m[1]
			var names []string
			for _, part := range strings.Split(list, ",") {
				// Strip parenthetical asides: "Suspended (any owned Platform suspended)".
				part = strings.TrimSpace(part)
				if i := strings.Index(part, "("); i >= 0 {
					part = strings.TrimSpace(part[:i])
				}
				if part != "" && part == strings.Title(part) && !strings.Contains(part, " ") { //nolint:staticcheck // ASCII phase names only
					names = append(names, part)
				}
			}
			if len(names) > 0 {
				out[e.Name()] = append(out[e.Name()], names...)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no `// Phase:` doc comments in the API package — this test would pass vacuously")
	}

	// Coverage guard. Parsing only what is annotated means a type that drops the
	// line stops being checked while the test still reports success — the same
	// shape as the defect this file exists to catch. Every declared phase field
	// must carry one.
	var declaredFields, annotated int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_types.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		declaredFields += strings.Count(string(b), "Phase string `json:\"phase,omitempty\"`")
		annotated += len(phaseDocRE.FindAllString(string(b), -1))
	}
	if declaredFields != annotated {
		t.Errorf("the API package declares %d phase fields but carries %d `// Phase: ...` lines.\n"+
			"      Every status type reporting a phase must name its values in that exact form,\n"+
			"      or this check silently stops covering it.", declaredFields, annotated)
	}
	return out
}

// TestAdvertisedPhasesAreNamedByTheOperator fails when a status doc comment
// promises a phase the operator has no constant for. "Drifted" was exactly
// that: documented on DatastoreStatus, defined nowhere, producible by nothing.
func TestAdvertisedPhasesAreNamedByTheOperator(t *testing.T) {
	declared := declaredPhaseConstants(t)
	// tenant_controller.go writes this one as a literal rather than via a
	// constant; it IS emitted, so it belongs to the vocabulary.
	declared["Active"] = true

	checked := 0
	for file, names := range advertisedPhases(t) {
		for _, n := range names {
			checked++
			if !declared[n] {
				keys := make([]string, 0, len(declared))
				for k := range declared {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				t.Errorf("%s advertises phase %q, which the operator defines nowhere.\n"+
					"      Known phases: %s\n"+
					"      A phase named only in a doc comment cannot be produced. Either add the\n"+
					"      constant and the code that sets it, or stop advertising it.",
					file, n, strings.Join(keys, ", "))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no advertised phases parsed — this test would pass vacuously")
	}
	t.Logf("checked %d advertised phase names against %d operator constants", checked, len(declared))
}

// TestDatastoreStatuses_PhaseMirrorsPlatform pins the invariant the doc comment
// now states: datastoreStatuses copies the Platform's phase verbatim, so a
// datastore can never report a phase the Platform cannot hold. This is the
// assertion that makes the vocabulary claim true rather than merely written.
func TestDatastoreStatuses_PhaseMirrorsPlatform(t *testing.T) {
	p := platformWithDatastores("acme",
		platformv1alpha1.DatastoreSpec{Name: "db", Kind: platformv1alpha1.DatastoreRelational},
		platformv1alpha1.DatastoreSpec{Name: "bucket", Kind: platformv1alpha1.DatastoreObjectStore},
	)

	for _, phase := range []string{phasePending, phaseProvisioning, phaseReady, phaseSuspended, phaseFailed} {
		got := datastoreStatuses(p, "development", testScope(), phase, nil)
		if len(got) != 2 {
			t.Fatalf("phase %s: got %d datastore statuses, want 2", phase, len(got))
		}
		for _, st := range got {
			if st.Phase != phase {
				t.Errorf("datastore %q: phase %q, want %q — the datastore phase is the "+
					"Platform's phase and must not diverge from it", st.Name, st.Phase, phase)
			}
		}
	}
}
