/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package main implements coverage-check: a coverage gate over a Go cover
// profile that enforces per-package floors and per-file overrides.
//
// It exists because `go tool cover` reports only an overall percentage and
// per-function numbers — it can neither fail a build below a floor nor pin a
// per-file 100% requirement on a security-critical path. The org testing
// rubric (nanohype/standards/testing-rubric.json) requires both: a coverage
// floor encoded in config so a regression fails rather than passes silently,
// and a per-file 100% override on security-critical code.
//
// Coverage is measured on the MERGED profile produced by
//
//	go test ./... -coverpkg=./internal/... -coverprofile=cover.out
//
// so the conformance (envtest) suite and the package unit tests both count
// toward each internal package's coverage. That merge places the same source
// block in the profile once per instrumented test binary; this tool merges
// duplicate blocks by taking the maximum hit count, exactly as
// `go tool cover -func` does, so the numbers match the standard tooling.
package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// modulePrefix is stripped from the profile's import-path-qualified file names
// to recover the on-disk path relative to the operators module root (the
// directory coverage-check runs from).
const modulePrefix = "github.com/nanohype/eks-agent-platform/operators/"

// ignoreMarker on a source line excludes any cover block overlapping that line
// from the uncovered set — for provably-unreachable defensive branches (e.g.
// json.Marshal of a statically-typed policy document, which cannot fail). Every
// use carries a reason after the marker; the report tallies ignored statements
// per file so the exclusions stay visible rather than silently swallowing
// coverage. This is the rubric's "exclusions listed explicitly" escape hatch,
// never a blanket disable.
const ignoreMarker = "//coverage:ignore"

// block is one profiled statement range: a byte-range within a file, the number
// of statements it spans, and whether any test hit it.
type block struct {
	startLine int
	endLine   int
	numStmts  int
	hit       bool
}

// fileCoverage accumulates a single source file's covered/total statements plus
// how many statements were excluded via ignoreMarker.
type fileCoverage struct {
	covered int
	total   int
	ignored int
}

func (fc fileCoverage) percent() float64 {
	if fc.total == 0 {
		return 100
	}
	return 100 * float64(fc.covered) / float64(fc.total)
}

// parseProfile reads a Go cover profile and returns per-file block lists, keyed
// by the on-disk path relative to the module root. Duplicate blocks (the same
// range emitted by more than one instrumented test binary in a -coverpkg merge)
// collapse to one, hit if any copy was hit — matching `go tool cover -func`.
func parseProfile(path string) (map[string][]block, error) {
	f, err := os.Open(path) //nolint:gosec // a dev tool reading the cover profile path passed on the command line
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// keyed by file -> "start.col,end.col" -> merged block
	merged := map[string]map[string]block{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("coverage-check: %s is not a cover profile (missing mode line)", path)
			}
			continue
		}
		if line == "" {
			continue
		}
		name, rng, num, count, err := parseProfileLine(line)
		if err != nil {
			return nil, err
		}
		file := strings.TrimPrefix(name, modulePrefix)
		startLine, endLine := lineSpan(rng)
		byRange := merged[file]
		if byRange == nil {
			byRange = map[string]block{}
			merged[file] = byRange
		}
		b := block{startLine: startLine, endLine: endLine, numStmts: num, hit: count > 0}
		if prev, ok := byRange[rng]; ok {
			b.hit = b.hit || prev.hit
		}
		byRange[rng] = b
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	out := map[string][]block{}
	for file, byRange := range merged {
		for _, b := range byRange {
			out[file] = append(out[file], b)
		}
	}
	return out, nil
}

// parseProfileLine splits one profile line
//
//	<import-path>/file.go:startLine.col,endLine.col numStmts count
//
// into its name, range, statement count, and hit count.
func parseProfileLine(line string) (name, rng string, num, count int, err error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", "", 0, 0, fmt.Errorf("coverage-check: malformed profile line %q", line)
	}
	colon := strings.LastIndex(fields[0], ":")
	if colon < 0 {
		return "", "", 0, 0, fmt.Errorf("coverage-check: malformed profile entry %q", fields[0])
	}
	name = fields[0][:colon]
	rng = fields[0][colon+1:]
	if num, err = strconv.Atoi(fields[1]); err != nil {
		return "", "", 0, 0, fmt.Errorf("coverage-check: bad statement count in %q: %w", line, err)
	}
	if count, err = strconv.Atoi(fields[2]); err != nil {
		return "", "", 0, 0, fmt.Errorf("coverage-check: bad hit count in %q: %w", line, err)
	}
	return name, rng, num, count, nil
}

// lineSpan extracts the [startLine, endLine] from a "startLine.col,endLine.col"
// range. On a malformed range it returns (0, 0), which never intersects an
// ignore annotation.
func lineSpan(rng string) (start, end int) {
	comma := strings.IndexByte(rng, ',')
	if comma < 0 {
		return 0, 0
	}
	start = leadingInt(rng[:comma])
	end = leadingInt(rng[comma+1:])
	return start, end
}

// leadingInt parses the integer before the first '.' in "line.col".
func leadingInt(s string) int {
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	n, _ := strconv.Atoi(s)
	return n
}

// ignoredLines returns the set of 1-based line numbers in a source file that
// carry the ignore marker. A missing file yields an empty set (the block simply
// isn't excluded), so a stale profile can never make the gate pass by accident.
func ignoredLines(file string) (map[int]bool, error) {
	f, err := os.Open(file) //nolint:gosec // reads a source file named by the cover profile
	if err != nil {
		if os.IsNotExist(err) {
			return map[int]bool{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[int]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		if strings.Contains(sc.Text(), ignoreMarker) {
			out[n] = true
		}
	}
	return out, sc.Err()
}

// ignoreResolver returns the annotated line set for a source file. Injected so
// the fold logic is unit-testable without touching the filesystem; production
// wires ignoredLines.
type ignoreResolver func(file string) (map[int]bool, error)

// coverageByFile folds the per-file blocks into covered/total/ignored counts,
// excluding any uncovered block that overlaps an ignoreMarker line.
func coverageByFile(blocks map[string][]block, resolve ignoreResolver) (map[string]fileCoverage, error) {
	out := map[string]fileCoverage{}
	for file, bs := range blocks {
		ignore, err := resolve(file)
		if err != nil {
			return nil, err
		}
		var fc fileCoverage
		for _, b := range bs {
			fc.total += b.numStmts
			switch {
			case b.hit:
				fc.covered += b.numStmts
			case overlapsIgnore(b, ignore):
				fc.covered += b.numStmts // treated as covered
				fc.ignored += b.numStmts
			}
		}
		out[file] = fc
	}
	return out, nil
}

// overlapsIgnore reports whether any line the block spans is annotated.
func overlapsIgnore(b block, ignore map[int]bool) bool {
	if len(ignore) == 0 {
		return false
	}
	for l := b.startLine; l <= b.endLine; l++ {
		if ignore[l] {
			return true
		}
	}
	return false
}

// packageOf returns the module-relative directory of a file (its Go package
// path), e.g. internal/controller.
func packageOf(file string) string {
	if slash := strings.LastIndex(file, "/"); slash >= 0 {
		return file[:slash]
	}
	return "."
}

// violation is a floor a package or file failed to clear.
type violation struct {
	scope string // package path or file path
	kind  string // "package" or "file"
	want  float64
	got   fileCoverage
}

// check applies the configured floors and returns any violations plus the
// per-package coverage it aggregated (for the report).
func check(fileCov map[string]fileCoverage, cfg config) ([]violation, map[string]fileCoverage) {
	pkgCov := map[string]fileCoverage{}
	for file, fc := range fileCov {
		p := packageOf(file)
		agg := pkgCov[p]
		agg.covered += fc.covered
		agg.total += fc.total
		agg.ignored += fc.ignored
		pkgCov[p] = agg
	}

	var vios []violation
	for pkg, floor := range cfg.packageFloors {
		fc, ok := pkgCov[pkg]
		if !ok {
			// A configured package with no measured statements is a config
			// drift (renamed/removed package) — fail loud rather than pass.
			vios = append(vios, violation{scope: pkg, kind: "package (no coverage data)", want: floor})
			continue
		}
		if fc.percent()+1e-9 < floor {
			vios = append(vios, violation{scope: pkg, kind: "package", want: floor, got: fc})
		}
	}
	for file, floor := range cfg.fileFloors {
		fc, ok := fileCov[file]
		if !ok {
			vios = append(vios, violation{scope: file, kind: "file (no coverage data)", want: floor})
			continue
		}
		if fc.percent()+1e-9 < floor {
			vios = append(vios, violation{scope: file, kind: "file", want: floor, got: fc})
		}
	}
	return vios, pkgCov
}

// config is the set of floors coverage-check enforces.
type config struct {
	// packageFloors maps a module-relative package path to its minimum
	// statement-coverage percentage.
	packageFloors map[string]float64
	// fileFloors maps a module-relative file path to a per-file override that
	// sits above its package floor (security-critical paths at 100).
	fileFloors map[string]float64
}

// report writes a human-readable coverage table to w.
func report(w *strings.Builder, pkgCov, fileCov map[string]fileCoverage, cfg config, vios []violation) {
	w.WriteString("coverage-check — per-package floors (statement coverage)\n")
	pkgs := make([]string, 0, len(pkgCov))
	for p := range pkgCov {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		fc := pkgCov[p]
		floor, gated := cfg.packageFloors[p]
		mark := "     "
		if gated {
			mark = fmt.Sprintf("≥%2.0f%% ", floor)
		}
		extra := ""
		if fc.ignored > 0 {
			extra = fmt.Sprintf("  (%d stmt ignored)", fc.ignored)
		}
		fmt.Fprintf(w, "  %s %6.1f%%  %4d/%-4d  %s%s\n", mark, fc.percent(), fc.covered, fc.total, p, extra)
	}
	if len(cfg.fileFloors) > 0 {
		w.WriteString("\nsecurity-critical files (per-file override):\n")
		files := make([]string, 0, len(cfg.fileFloors))
		for f := range cfg.fileFloors {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			fc := fileCov[f]
			extra := ""
			if fc.ignored > 0 {
				extra = fmt.Sprintf("  (%d stmt ignored)", fc.ignored)
			}
			fmt.Fprintf(w, "  ≥%3.0f%% %6.1f%%  %4d/%-4d  %s%s\n", cfg.fileFloors[f], fc.percent(), fc.covered, fc.total, f, extra)
		}
	}
	if len(vios) == 0 {
		w.WriteString("\nOK — every floor cleared.\n")
		return
	}
	w.WriteString("\nFAIL — coverage floors not met:\n")
	for _, v := range vios {
		if strings.Contains(v.kind, "no coverage data") {
			fmt.Fprintf(w, "  %-8s %-45s no coverage data (want ≥%.0f%%)\n", v.kind, v.scope, v.want)
			continue
		}
		fmt.Fprintf(w, "  %-8s %-45s %.1f%% < %.0f%%  (%d/%d)\n", v.kind, v.scope, v.got.percent(), v.want, v.got.covered, v.got.total)
	}
}
