/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The eval WorkflowTemplate's steps are shell scripts that Argo assembles from
// the template text, and what a step does with a value depends on where Argo
// puts it. These tests run each step the way Argo delivers it — expressions
// substituted, raw input artifacts written to their paths, env values set — under
// /bin/sh, with a stub ahead of PATH for every command that reaches the world.
// What a script is observed to do settles questions a reading of it cannot,
// because a reading can be defeated by how the script is written and an
// execution cannot.

// evalStep is the part of a WorkflowTemplate step Argo turns into a pod: the
// inputs it substitutes, the raw artifacts it writes before the script starts,
// and the script with its environment.
type evalStep struct {
	Name   string `json:"name"`
	Inputs struct {
		Parameters []struct {
			Name string `json:"name"`
		} `json:"parameters"`
		Artifacts []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Raw  *struct {
				Data string `json:"data"`
			} `json:"raw"`
		} `json:"artifacts"`
	} `json:"inputs"`
	Outputs struct {
		Parameters []struct {
			Name      string `json:"name"`
			ValueFrom struct {
				Path string `json:"path"`
			} `json:"valueFrom"`
		} `json:"parameters"`
	} `json:"outputs"`
	Script *struct {
		Source string `json:"source"`
		Env    []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
	} `json:"script"`
}

// evalScriptSteps returns every script step of the WorkflowTemplate by name.
func evalScriptSteps(t *testing.T) map[string]evalStep {
	t.Helper()
	var doc struct {
		Spec struct {
			Templates []evalStep `json:"templates"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readEvalWorkflowTemplate(t)), &doc); err != nil {
		t.Fatalf("parse the WorkflowTemplate: %v", err)
	}
	steps := map[string]evalStep{}
	for _, s := range doc.Spec.Templates {
		if s.Script != nil {
			steps[s.Name] = s
		}
	}
	if len(steps) == 0 {
		t.Fatal("parsed no script step out of the WorkflowTemplate; every check below would pass vacuously")
	}
	return steps
}

// argoExpression matches an Argo simple-template expression and captures its name.
var argoExpression = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// stubbedCommands are the commands the eval steps reach S3, the cluster and the
// model through, plus jq, which every step pipes through.
var stubbedCommands = []string{"aws", "kubectl", "evaluate", "score", "jq"}

// stepRun is what one execution of a step left behind.
type stepRun struct {
	// tmp is where the step's /tmp/ landed.
	tmp string
	// calls holds the argv of every stubbed command the script ran, in order,
	// with the command's name first.
	calls [][]string
}

// commands returns the recorded calls of one stubbed command.
func (r stepRun) commands(name string) [][]string {
	var out [][]string
	for _, c := range r.calls {
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

// flagValue returns the argument that follows flag in argv, and whether there was one.
func flagValue(argv []string, flag string) (string, bool) {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1], true
		}
	}
	return "", false
}

// runEvalStep executes one step as Argo would deliver it, with values naming
// the result of every expression the step references. An expression with no
// value fails the test: Argo would resolve it, so a run that left it in place
// would not be Argo's.
//
// The step's own /tmp/ paths are moved under a directory of the test's, in the
// template text and before substitution, so a substituted value is never
// rewritten. The script exits zero or the test fails: a step that stopped
// partway has not run the lines after the one that stopped it.
func runEvalStep(t *testing.T, step evalStep, values map[string]string) stepRun {
	t.Helper()
	if step.Script == nil {
		t.Fatalf("step %s carries no script", step.Name)
	}
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	calls := filepath.Join(root, "calls")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{tmp, calls, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("make %s: %v", d, err)
		}
	}

	relocate := func(s string) string { return strings.ReplaceAll(s, "/tmp/", tmp+"/") }
	unresolved := map[string]bool{}
	resolve := func(s string) string {
		return argoExpression.ReplaceAllStringFunc(s, func(m string) string {
			name := argoExpression.FindStringSubmatch(m)[1]
			v, ok := values[name]
			if !ok {
				unresolved[name] = true
				return m
			}
			return v
		})
	}

	type file struct{ path, data string }
	var artifacts []file
	for _, a := range step.Inputs.Artifacts {
		if a.Raw == nil {
			t.Fatalf("step %s declares input artifact %q with no raw source; this harness delivers raw artifacts only", step.Name, a.Name)
		}
		artifacts = append(artifacts, file{relocate(a.Path), resolve(a.Raw.Data)})
	}
	env := []string{"PATH=" + bin + ":/usr/bin:/bin"}
	for _, e := range step.Script.Env {
		env = append(env, e.Name+"="+resolve(relocate(e.Value)))
	}
	source := resolve(relocate(step.Script.Source))
	if len(unresolved) > 0 {
		names := make([]string, 0, len(unresolved))
		for n := range unresolved {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("step %s references %s, which this run was given no value for", step.Name, strings.Join(names, ", "))
	}

	for _, a := range artifacts {
		if err := os.WriteFile(a.path, []byte(a.data), 0o644); err != nil {
			t.Fatalf("write input artifact %s: %v", a.path, err)
		}
	}
	// Each stub records its argv NUL-separated, one file per call, numbered so a
	// directory listing reads in call order. jq prints the file it was handed as
	// its last argument when there is one, so a document passes through a step's
	// jq unchanged, and an empty object otherwise.
	for _, name := range stubbedCommands {
		stub := "#!/bin/sh\n" +
			"n=$(ls '" + calls + "' | wc -l | tr -d ' ')\n" +
			"printf '%s\\0' '" + name + "' \"$@\" > '" + calls + "'/$(printf '%04d' \"$n\")\n"
		if name == "jq" {
			stub += "for last; do :; done\n" +
				"if [ -n \"${last-}\" ] && [ -f \"$last\" ]; then cat \"$last\"; else printf '{}\\n'; fi\n"
		}
		stub += "exit 0\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatalf("write the %s stub: %v", name, err)
		}
	}

	script := filepath.Join(root, "script")
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatalf("write the %s script: %v", step.Name, err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Dir = root
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("step %s did not run to completion: %v\n%s", step.Name, err, out)
	}

	entries, err := os.ReadDir(calls)
	if err != nil {
		t.Fatalf("read the recorded calls: %v", err)
	}
	run := stepRun{tmp: tmp}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(calls, e.Name()))
		if err != nil {
			t.Fatalf("read a recorded call: %v", err)
		}
		run.calls = append(run.calls, strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00"))
	}
	return run
}

// stepValues gives every expression a step references a value: the payload for
// every input parameter and workflow parameter, and a well-formed value for the
// two Argo-generated globals the template keeps in source.
func stepValues(step evalStep, payload string) map[string]string {
	values := map[string]string{
		"workflow.uid":                       "6f1c9d2e-3b4a-4c5d-8e7f-9a0b1c2d3e4f",
		"workflow.creationTimestamp.RFC3339": "2026-01-01T00:00:00Z",
	}
	for _, p := range step.Inputs.Parameters {
		values["inputs.parameters."+p.Name] = payload
	}
	texts := []string{step.Script.Source}
	for _, e := range step.Script.Env {
		texts = append(texts, e.Value)
	}
	for _, text := range texts {
		for _, m := range argoExpression.FindAllStringSubmatch(text, -1) {
			if strings.HasPrefix(m[1], "workflow.parameters.") {
				values[m[1]] = payload
			}
		}
	}
	return values
}

// TestNoStepRunsAParameterValueAsCode delivers a hostile value through every
// input of every step and runs the step.
//
// The values these steps handle include tenant-authored case inputs, the
// EvalSuite's manifest name and the model's replies to adversarial cases, and
// the step pods hold the eval-runner role and kubectl. A value that reaches a
// script as source runs there: written into the score step as
//
//	echo '<results-json, substituted by Argo>' > /tmp/results.json
//
// a reply holding `'; touch /tmp/pwned; echo '` runs the touch. Each payload
// here names a marker file of its own, and a marker that exists after the run
// names the step and the vector that ran it.
//
// Two more properties hold the delivery to being data rather than merely inert:
// a value reaches a command as whole arguments, never split by the shell; and the
// documents a step hands to a command arrive byte for byte, backslashes
// included, which dash's echo would have rewritten.
func TestNoStepRunsAParameterValueAsCode(t *testing.T) {
	steps := evalScriptSteps(t)
	markers := t.TempDir()

	attacks := []struct{ name, body string }{
		{"single-quote-breakout", `'; touch %s; echo '`},
		{"double-quote-breakout", `"; touch %s; echo "`},
		{"command-substitution", `$(touch %s)`},
		{"backticks", "`touch %s`"},
		{"newline", "\ntouch %s\n"},
	}
	// Where a step hands a document to a command: the command, and the flag
	// naming the file ("" for its last argument).
	documents := map[string]struct{ command, flag string }{
		"run-cases": {"evaluate", "--cases"},
		"score":     {"score", "--results"},
		"writeback": {"jq", ""},
	}

	names := make([]string, 0, len(steps))
	for n := range steps {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		step := steps[name]
		for _, a := range attacks {
			t.Run(name+"/"+a.name, func(t *testing.T) {
				t.Parallel()
				marker := filepath.Join(markers, name+"-"+a.name)
				payload := "PAYLOAD[" + fmt.Sprintf(a.body, marker) + ` \n \c \\ ]PAYLOAD`

				variants := []map[string]string{stepValues(step, payload)}
				if name == "resolve-cases" {
					// The manifest branches run only when there are no inline cases.
					for _, manifest := range []string{payload, "s3://" + payload} {
						v := stepValues(step, payload)
						v["inputs.parameters.cases-inline"] = ""
						v["inputs.parameters.cases-manifest"] = manifest
						variants = append(variants, v)
					}
				}

				for i, values := range variants {
					run := runEvalStep(t, step, values)
					if _, err := os.Stat(marker); err == nil {
						t.Errorf("step %s ran a value as code (%s, variant %d): %s exists", name, a.name, i, marker)
						_ = os.Remove(marker)
					}
					assertWholeArguments(t, name, run, payload)

					if name == "resolve-cases" && i == 0 {
						assertOutputHolds(t, step, run, "cases", payload)
					}
					if d, ok := documents[name]; ok {
						assertDocumentHanded(t, name, run, d.command, d.flag, payload)
					}
				}
			})
		}
	}
}

// assertWholeArguments fails when a stubbed command received part of the
// payload in an argument that does not hold all of it — the shell split it.
func assertWholeArguments(t *testing.T, step string, run stepRun, payload string) {
	t.Helper()
	for _, call := range run.calls {
		for _, arg := range call[1:] {
			whole := strings.Count(arg, payload)
			if strings.Count(arg, "PAYLOAD[") != whole || strings.Count(arg, "]PAYLOAD") != whole {
				t.Errorf("step %s handed %s a split value: argument %q holds part of the payload", step, call[0], arg)
			}
		}
	}
}

// assertOutputHolds fails unless the step's output parameter file holds want.
func assertOutputHolds(t *testing.T, step evalStep, run stepRun, output, want string) {
	t.Helper()
	for _, p := range step.Outputs.Parameters {
		if p.Name != output {
			continue
		}
		path := strings.ReplaceAll(p.ValueFrom.Path, "/tmp/", run.tmp+"/")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("step %s left no %s output at %s: %v", step.Name, output, p.ValueFrom.Path, err)
		}
		if string(got) != want {
			t.Errorf("step %s output %s = %q, want the input document byte for byte: %q", step.Name, output, got, want)
		}
		return
	}
	t.Fatalf("step %s declares no output parameter %s", step.Name, output)
}

// assertDocumentHanded fails unless command was handed a file holding want.
func assertDocumentHanded(t *testing.T, step string, run stepRun, command, flag, want string) {
	t.Helper()
	calls := run.commands(command)
	if len(calls) == 0 {
		t.Fatalf("step %s never ran %s", step, command)
	}
	argv := calls[0]
	path, ok := argv[len(argv)-1], true
	if flag != "" {
		path, ok = flagValue(argv, flag)
	}
	if !ok {
		t.Fatalf("step %s ran %s without %s: %q", step, command, flag, argv)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("step %s handed %s %q, which is not a readable file: %v", step, command, path, err)
	}
	if string(got) != want {
		t.Errorf("step %s handed %s a document that differs from its input:\n got %q\nwant %q", step, command, got, want)
	}
}
