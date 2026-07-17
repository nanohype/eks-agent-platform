/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return p
}

// noIgnore is the resolver used when a test doesn't exercise ignore handling.
func noIgnore(string) (map[int]bool, error) { return map[int]bool{}, nil }

func TestParseProfile_MergesDuplicateBlocksByMaxHit(t *testing.T) {
	// The same block appears twice (as it would from two instrumented test
	// binaries in a -coverpkg merge): hit in one copy, missed in the other.
	// The merged result must count it as hit — matching `go tool cover -func`.
	prof := writeProfile(t, "mode: set\n"+
		modulePrefix+"internal/controller/x.go:10.2,12.3 2 0\n"+
		modulePrefix+"internal/controller/x.go:10.2,12.3 2 1\n"+
		modulePrefix+"internal/controller/x.go:20.2,20.10 1 0\n")

	blocks, err := parseProfile(prof)
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	fc, err := coverageByFile(blocks, noIgnore)
	if err != nil {
		t.Fatalf("coverageByFile: %v", err)
	}
	got := fc["internal/controller/x.go"]
	if got.total != 3 {
		t.Errorf("total statements: got %d want 3 (duplicate range counted once)", got.total)
	}
	if got.covered != 2 {
		t.Errorf("covered: got %d want 2 (merged block hit; the 1-stmt block missed)", got.covered)
	}
}

func TestCoverageByFile_IgnoreMarkerExcludesUnreachableBlock(t *testing.T) {
	prof := writeProfile(t, "mode: set\n"+
		modulePrefix+"internal/controller/x.go:10.2,10.20 1 1\n"+ // covered
		modulePrefix+"internal/controller/x.go:20.16,22.3 1 0\n") // uncovered, will be ignored

	// Line 21 (inside the 20→22 block) carries the marker.
	resolve := func(string) (map[int]bool, error) { return map[int]bool{21: true}, nil }
	fc, err := coverageByFile(mustBlocks(t, prof), resolve)
	if err != nil {
		t.Fatalf("coverageByFile: %v", err)
	}
	got := fc["internal/controller/x.go"]
	if got.percent() != 100 {
		t.Errorf("ignored unreachable block must count as covered: got %.1f%% want 100", got.percent())
	}
	if got.ignored != 1 {
		t.Errorf("ignored tally: got %d want 1", got.ignored)
	}
}

func TestCoverageByFile_UnignoredUncoveredCountsAgainst(t *testing.T) {
	prof := writeProfile(t, "mode: set\n"+
		modulePrefix+"internal/controller/x.go:10.2,10.20 1 1\n"+
		modulePrefix+"internal/controller/x.go:20.16,22.3 1 0\n")
	// Marker on line 30 — outside the uncovered block's 20..22 span, so it must
	// NOT exclude it.
	resolve := func(string) (map[int]bool, error) { return map[int]bool{30: true}, nil }
	fc, err := coverageByFile(mustBlocks(t, prof), resolve)
	if err != nil {
		t.Fatalf("coverageByFile: %v", err)
	}
	if got := fc["internal/controller/x.go"]; got.percent() != 50 {
		t.Errorf("a marker outside the block span must not exclude it: got %.1f%% want 50", got.percent())
	}
}

func TestCheck_PackageFloorAndSecurityFileOverride(t *testing.T) {
	fileCov := map[string]fileCoverage{
		// controller aggregate = (8+2)/(10+4) = 71.4% -> below 75 floor.
		"internal/controller/a.go":            {covered: 8, total: 10},
		"internal/controller/platform_iam.go": {covered: 2, total: 4}, // 50% -> below 100 override
		"internal/operatorconfig/config.go":   {covered: 40, total: 50},
		"internal/agentctl/commands.go":       {covered: 10, total: 100}, // 10% but floor is 30 -> violation
		"internal/awsclients/clients.go":      {covered: 5, total: 10},   // 50% >= 30 floor -> ok
	}
	cfg := config{
		packageFloors: map[string]float64{
			"internal/controller":     75,
			"internal/operatorconfig": 75,
			"internal/agentctl":       30,
			"internal/awsclients":     30,
		},
		fileFloors: map[string]float64{
			"internal/controller/platform_iam.go": 100,
		},
	}
	vios, pkg := check(fileCov, cfg)

	if p := pkg["internal/operatorconfig"]; p.percent() != 80 {
		t.Errorf("operatorconfig percent: got %.1f want 80", p.percent())
	}
	scopes := map[string]bool{}
	for _, v := range vios {
		scopes[v.scope] = true
	}
	for _, want := range []string{"internal/controller", "internal/controller/platform_iam.go", "internal/agentctl"} {
		if !scopes[want] {
			t.Errorf("expected a violation for %q; got %v", want, scopes)
		}
	}
	if scopes["internal/operatorconfig"] {
		t.Error("operatorconfig at 80%% should clear its 75%% floor")
	}
	if scopes["internal/awsclients"] {
		t.Error("awsclients at 50%% should clear its 30%% floor")
	}
}

func TestCheck_ConfiguredPackageWithNoDataFailsLoud(t *testing.T) {
	// A renamed/removed package that the config still names must fail rather
	// than silently pass — otherwise a floor could be evaded by deleting code.
	cfg := config{packageFloors: map[string]float64{"internal/ghost": 75}}
	vios, _ := check(map[string]fileCoverage{}, cfg)
	if len(vios) != 1 || !strings.Contains(vios[0].kind, "no coverage data") {
		t.Fatalf("expected a no-coverage-data violation, got %+v", vios)
	}
}

func TestParseProfile_RejectsNonProfile(t *testing.T) {
	p := writeProfile(t, "this is not a cover profile\n")
	if _, err := parseProfile(p); err == nil {
		t.Fatal("expected an error for a file without a mode line")
	}
}

func TestIgnoredLines_ReadsMarkers(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.go")
	if err := os.WriteFile(src, []byte("package x\nfoo() //coverage:ignore because\nbar()\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ignoredLines(src)
	if err != nil {
		t.Fatalf("ignoredLines: %v", err)
	}
	if !got[2] || got[3] {
		t.Errorf("ignoredLines: got %v want only line 2", got)
	}
	// A missing file is an empty set, not an error (a stale profile can't make
	// the gate pass by pointing at a vanished source path).
	missing, err := ignoredLines(filepath.Join(dir, "gone.go"))
	if err != nil || len(missing) != 0 {
		t.Errorf("missing file: got (%v, %v) want (empty, nil)", missing, err)
	}
}

func TestPackageOf(t *testing.T) {
	if got := packageOf("internal/controller/x.go"); got != "internal/controller" {
		t.Errorf("packageOf: got %q", got)
	}
	if got := packageOf("main.go"); got != "." {
		t.Errorf("packageOf top-level: got %q", got)
	}
}

func mustBlocks(t *testing.T, prof string) map[string][]block {
	t.Helper()
	b, err := parseProfile(prof)
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	return b
}
