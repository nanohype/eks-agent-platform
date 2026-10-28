/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The eval WorkflowTemplate's steps are shell scripts that Argo assembles from
// the template text, and what a step does with a value depends on where Argo
// puts it. These tests run each step the way Argo delivers it — expressions
// substituted, raw input artifacts written to their paths, env values set —
// with each of stubbedCommands replaced by a stub ahead of PATH. What a script
// is observed to do settles questions a reading of it cannot, because a reading
// can be defeated by how the script is written and an execution cannot.
//
// Two limits on what a passing run shows. The eval-runner image is built on
// Debian, so a step's /bin/sh is dash; the harness runs each step under dash
// when the host has it on PATH and under the host's /bin/sh otherwise, which on
// a host without dash is a different shell from the step's. And Argo
// substitutes over the template's JSON form in two passes, globals and then the
// step's inputs; the harness substitutes each expression once, so a value that
// itself holds an expression is delivered here as written.

// evalStep is the part of a WorkflowTemplate step Argo turns into a pod: the
// inputs it substitutes, the raw artifacts it writes before the script starts,
// and the script with its environment.
type evalStep struct {
	Name   string `json:"name"`
	Inputs struct {
		Parameters []struct {
			Name string `json:"name"`
			// Value is the default an input takes when no argument binds it.
			Value *string `json:"value"`
		} `json:"parameters"`
		Artifacts []struct {
			Name    string `json:"name"`
			Path    string `json:"path"`
			Archive *struct {
				None *struct{} `json:"none"`
			} `json:"archive"`
			Raw *struct {
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

// argoExpression matches an Argo template tag: from a `{{` to the first `}}`
// after it, across lines. scripts/check-argo-script-splices.py reads tags with
// the same grammar, and TestArgoTagGrammarMatchesTheGate holds the two to it.
var argoExpression = regexp.MustCompile(`(?s)\{\{(.*?)\}\}`)

// expressionName is the name a tag holds, trimmed of the space around it.
func expressionName(tag string) string {
	return strings.TrimSpace(argoExpression.FindStringSubmatch(tag)[1])
}

// substitute replaces every expression in s with its value in values, and
// returns the names it found no value for, which it leaves in place.
func substitute(s string, values map[string]string) (string, []string) {
	var missing []string
	out := argoExpression.ReplaceAllStringFunc(s, func(tag string) string {
		name := expressionName(tag)
		v, ok := values[name]
		if !ok {
			missing = append(missing, name)
			return tag
		}
		return v
	})
	return out, missing
}

// stubbedCommands are the commands the eval steps reach S3, the cluster and the
// model through, plus jq, which every step pipes through.
var stubbedCommands = []string{"aws", "kubectl", "evaluate", "score", "jq"}

// testWorkflowUID is the uid every run here gives the Workflow.
const testWorkflowUID = "6f1c9d2e-3b4a-4c5d-8e7f-9a0b1c2d3e4f"

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

// stepShell is the shell a step runs under here: dash, the eval-runner image's
// /bin/sh, when the host has it, and the host's /bin/sh otherwise.
func stepShell() string {
	if p, err := exec.LookPath("dash"); err == nil {
		return p
	}
	return "/bin/sh"
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
		out, missing := substitute(s, values)
		for _, name := range missing {
			unresolved[name] = true
		}
		return out
	}

	type file struct{ path, data string }
	artifacts := make([]file, 0, len(step.Inputs.Artifacts))
	for _, a := range step.Inputs.Artifacts {
		if a.Raw == nil {
			t.Fatalf("step %s declares input artifact %q with no raw source; this harness delivers raw artifacts only", step.Name, a.Name)
		}
		if a.Archive == nil || a.Archive.None == nil {
			t.Fatalf("step %s input artifact %q sets no archive: {none: {}}. This harness writes raw data to the "+
				"path byte for byte, which is Argo's delivery under archive.none only", step.Name, a.Name)
		}
		artifacts = append(artifacts, file{relocate(a.Path), resolve(a.Raw.Data)})
	}
	env := make([]string, 0, 1+len(step.Script.Env))
	env = append(env, "PATH="+bin+":/usr/bin:/bin")
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
	cmd := exec.Command(stepShell(), script)
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

// payloadFor is the payload the expression name is given: the attack, tagged
// with the name, so a value delivered in another's place is told apart from
// its own.
func payloadFor(name, attack string) string {
	return "PAYLOAD[" + name + ":" + attack + "]PAYLOAD"
}

// stepValues gives every expression a step references a value: a payload of
// its own for each input parameter and each workflow parameter, and a
// well-formed value for the two Argo-generated globals the template keeps in
// source.
func stepValues(step evalStep, attack string) map[string]string {
	values := map[string]string{
		"workflow.uid":                       testWorkflowUID,
		"workflow.creationTimestamp.RFC3339": "2026-01-01T00:00:00Z",
	}
	for _, p := range step.Inputs.Parameters {
		name := "inputs.parameters." + p.Name
		values[name] = payloadFor(name, attack)
	}
	texts := make([]string, 0, 1+len(step.Script.Env)+len(step.Inputs.Artifacts))
	texts = append(texts, step.Script.Source)
	for _, e := range step.Script.Env {
		texts = append(texts, e.Value)
	}
	for _, a := range step.Inputs.Artifacts {
		if a.Raw != nil {
			texts = append(texts, a.Raw.Data)
		}
	}
	for _, text := range texts {
		for _, tag := range argoExpression.FindAllString(text, -1) {
			if name := expressionName(tag); strings.HasPrefix(name, "workflow.parameters.") {
				values[name] = payloadFor(name, attack)
			}
		}
	}
	return values
}

// evalPipeline is what hands each step its inputs: the DAG's tasks with the
// arguments each passes its template, and the exit handler.
type evalPipeline struct {
	Spec struct {
		OnExit    string `json:"onExit"`
		Templates []struct {
			DAG *struct {
				Tasks []struct {
					Name      string `json:"name"`
					Template  string `json:"template"`
					Arguments struct {
						Parameters []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"parameters"`
					} `json:"arguments"`
				} `json:"tasks"`
			} `json:"dag"`
		} `json:"templates"`
	} `json:"spec"`
}

// wiredValues resolves, for each step the pipeline runs, the value of every
// expression the step references, the way Argo reaches it from a run's
// globals: a task's arguments, resolved against globals, bind the step's input
// parameters, and an input no argument binds takes its default, resolved the
// same way. The exit handler's inputs are all defaults. globals holds the
// workflow-scoped values and the task outputs a DAG argument reads.
func wiredValues(t *testing.T, globals map[string]string) map[string]map[string]string {
	t.Helper()
	steps := evalScriptSteps(t)
	var doc evalPipeline
	if err := yaml.Unmarshal([]byte(readEvalWorkflowTemplate(t)), &doc); err != nil {
		t.Fatalf("parse the WorkflowTemplate: %v", err)
	}
	out := map[string]map[string]string{}
	bind := func(caller, template string, args map[string]string) {
		step, ok := steps[template]
		if !ok {
			t.Fatalf("%s runs template %q, which is no script step", caller, template)
		}
		values := maps.Clone(globals)
		declared := map[string]bool{}
		for _, p := range step.Inputs.Parameters {
			declared[p.Name] = true
			text, ok := args[p.Name]
			if !ok && p.Value != nil {
				text, ok = *p.Value, true
			}
			if !ok {
				t.Fatalf("%s binds no value to input %q of step %s, which declares no default", caller, p.Name, template)
			}
			v, missing := substitute(text, globals)
			if len(missing) > 0 {
				t.Fatalf("input %q of step %s references %s, which this run was given no value for",
					p.Name, template, strings.Join(missing, ", "))
			}
			values["inputs.parameters."+p.Name] = v
		}
		for name := range args {
			if !declared[name] {
				t.Errorf("%s passes %q to step %s, which declares no such input", caller, name, template)
			}
		}
		out[template] = values
	}
	for _, tmpl := range doc.Spec.Templates {
		if tmpl.DAG == nil {
			continue
		}
		for _, task := range tmpl.DAG.Tasks {
			args := make(map[string]string, len(task.Arguments.Parameters))
			for _, a := range task.Arguments.Parameters {
				args[a.Name] = a.Value
			}
			bind("task "+task.Name, task.Template, args)
		}
	}
	if doc.Spec.OnExit == "" {
		t.Fatal("the WorkflowTemplate names no onExit handler")
	}
	bind("onExit", doc.Spec.OnExit, nil)
	return out
}

// wiringGlobals gives every value a run's steps reach a distinct value of its
// own, so a value delivered in another's place is told apart from its own. The
// run ended Failed, so the exit handler writes its outcome.
func wiringGlobals() map[string]string {
	return map[string]string{
		"workflow.parameters.platform":             "acme",
		"workflow.parameters.tenant":               "acme-tenant",
		"workflow.parameters.fleet":                "fleet-one",
		"workflow.parameters.suite-name":           "nightly",
		"workflow.parameters.suite-namespace":      testSuiteNamespace,
		"workflow.parameters.pass-threshold":       "0.85",
		"workflow.parameters.cases-inline":         "",
		"workflow.parameters.cases-manifest":       "smoke.json",
		"workflow.parameters.eval-reports-bucket":  testReportsBucket,
		"workflow.parameters.model-route-base-url": "http://gateway.acme.svc:8080/anthropic",
		"workflow.parameters.model-route":          "acme-claude",
		"workflow.parameters.model-route-api":      "anthropic",
		"workflow.name":                            "acme-nightly-x7k2p",
		"workflow.status":                          "Failed",
		"workflow.uid":                             testWorkflowUID,
		"workflow.creationTimestamp.RFC3339":       "2026-01-01T00:00:00Z",
		"tasks.resolve.outputs.parameters.cases":   `[{"name":"greets","input":"hi"}]`,
		"tasks.run.outputs.parameters.results":     `[{"name":"greets","output":"hello"}]`,
		"tasks.score.outputs.parameters.score":     `{"meanScore":"0.92","passed":true,"reportUrl":"s3://r/report.html"}`,
	}
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
// included, which dash's echo would have rewritten. Each input carries a payload
// of its own, so the document a command receives is the one its input names.
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
	// Where a step hands a document to a command: the command, the flag naming
	// the file ("" for its last argument), and the input the document comes from.
	documents := map[string]struct{ command, flag, input string }{
		"run-cases": {"evaluate", "--cases", "cases-json"},
		"score":     {"score", "--results", "results-json"},
		"writeback": {"jq", "", "score-json"},
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
				attack := fmt.Sprintf(a.body, marker) + ` \n \c \\ `

				variants := []map[string]string{stepValues(step, attack)}
				if name == "resolve-cases" {
					// The manifest branches run only when there are no inline cases.
					for _, scheme := range []string{"", "s3://"} {
						v := stepValues(step, attack)
						v["inputs.parameters.cases-inline"] = ""
						v["inputs.parameters.cases-manifest"] = scheme + v["inputs.parameters.cases-manifest"]
						variants = append(variants, v)
					}
				}

				for i, values := range variants {
					run := runEvalStep(t, step, values)
					if _, err := os.Stat(marker); err == nil {
						t.Errorf("step %s ran a value as code (%s, variant %d): %s exists", name, a.name, i, marker)
						_ = os.Remove(marker)
					}
					assertWholeArguments(t, name, run, values, attack)

					if name == "resolve-cases" && i == 0 {
						assertOutputHolds(t, step, run, "cases", values["inputs.parameters.cases-inline"])
					}
					if d, ok := documents[name]; ok {
						assertDocumentHanded(t, name, run, d.command, d.flag, values["inputs.parameters."+d.input])
					}
				}
			})
		}
	}
}

// TestEachStepReceivesItsOwnValues runs the steps with the values the pipeline
// binds to them, each distinct, and checks each value reaches the argument it
// belongs in. A scalar reaches its script through an env entry naming an
// input, and an input through a task argument naming a workflow parameter or a
// task output. A swap at either layer hands a well-formed value to the wrong
// place, and nothing runs as code, so only a check on where each value lands
// sees it. The status-writing steps are TestWritebackPatchesTheSuiteNamespace's.
func TestEachStepReceivesItsOwnValues(t *testing.T) {
	steps := evalScriptSteps(t)
	g := wiringGlobals()
	wired := wiredValues(t, g)
	bucket, platform := g["workflow.parameters.eval-reports-bucket"], g["workflow.parameters.platform"]

	t.Run("resolve-cases/manifest", func(t *testing.T) {
		run := runEvalStep(t, steps["resolve-cases"], wired["resolve-cases"])
		want := []string{"aws", "s3", "cp", "s3://" + bucket + "/" + platform + "/manifests/" + g["workflow.parameters.cases-manifest"]}
		calls := run.commands("aws")
		if len(calls) != 1 || len(calls[0]) < len(want) || !reflect.DeepEqual(calls[0][:len(want)], want) {
			t.Errorf("the resolve-cases step fetched its manifest with %q, want one call beginning %q", calls, want)
		}
	})
	t.Run("resolve-cases/inline", func(t *testing.T) {
		inline := wiringGlobals()
		inline["workflow.parameters.cases-inline"] = `[{"name":"inline-only"}]`
		run := runEvalStep(t, steps["resolve-cases"], wiredValues(t, inline)["resolve-cases"])
		if calls := run.commands("aws"); len(calls) != 0 {
			t.Errorf("the resolve-cases step was handed inline cases and still fetched a manifest: %q", calls)
		}
		assertOutputHolds(t, steps["resolve-cases"], run, "cases", inline["workflow.parameters.cases-inline"])
	})
	t.Run("run-cases", func(t *testing.T) {
		run := runEvalStep(t, steps["run-cases"], wired["run-cases"])
		assertFlags(t, "run-cases", run, "evaluate", map[string]string{
			"--base-url":  g["workflow.parameters.model-route-base-url"],
			"--route":     g["workflow.parameters.model-route"],
			"--route-api": g["workflow.parameters.model-route-api"],
		})
		assertDocumentHanded(t, "run-cases", run, "evaluate", "--cases", g["tasks.resolve.outputs.parameters.cases"])
	})
	t.Run("score", func(t *testing.T) {
		run := runEvalStep(t, steps["score"], wired["score"])
		assertFlags(t, "score", run, "score", map[string]string{
			"--pass-threshold": g["workflow.parameters.pass-threshold"],
		})
		assertDocumentHanded(t, "score", run, "score", "--results", g["tasks.run.outputs.parameters.results"])
		// The eval-runner role is granted writes under <bucket>/*/runs/ only, and
		// the platform segment is what keeps one platform's reports from another's.
		prefix := "s3://" + bucket + "/" + platform + "/runs/" + g["workflow.name"] + "/"
		uploads := run.commands("aws")
		files := []string{"report.html", "junit.xml"}
		if len(uploads) != len(files) {
			t.Fatalf("the score step made %d aws call(s), want an upload of each of %q: %q", len(uploads), files, uploads)
		}
		for i, f := range files {
			dest := uploads[i][len(uploads[i])-1]
			if !strings.HasPrefix(dest, prefix) || !strings.HasSuffix(dest, "-"+testWorkflowUID+"/"+f) {
				t.Errorf("the score step uploads %s to %q, want %s<time>-%s/%s", f, dest, prefix, testWorkflowUID, f)
			}
		}
	})
}

// TestArgoTagGrammarMatchesTheGate holds this harness and
// scripts/check-argo-script-splices.py to one tag grammar. The gate decides
// which expressions sit in code, and the harness decides which it resolves. A
// tag one reads and the other does not is either a splice the gate passes over
// or an expression the harness leaves in place, and each is a check that
// passes without having looked.
//
// The gate's grammar is run, not read: python3 loads the gate and applies its
// EXPRESSION pattern to the texts argoExpression reads here, the
// WorkflowTemplate among them. Loading the gate needs no third-party module.
func TestArgoTagGrammarMatchesTheGate(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		// CI sets REQUIRE_PYTHON3, so a runner without python3 fails here
		// rather than reporting a comparison it never made.
		if os.Getenv("REQUIRE_PYTHON3") != "" {
			t.Fatal("REQUIRE_PYTHON3 is set but python3 is not on PATH; the two tag grammars were not compared")
		}
		t.Skip("python3 not on PATH, so the gate's tag grammar cannot be run beside this harness's")
	}

	cases := []struct{ what, text string }{
		{"the WorkflowTemplate", readEvalWorkflowTemplate(t)},
		{"a quoted splice", `echo '{{inputs.parameters.x}}'`},
		{"inner spaces", `{{ inputs.parameters.x }}`},
		{"a tag across lines", "{{\n\tinputs.parameters.x\n}}"},
		{"an expr-language tag", `{{=inputs.parameters.x}}`},
		{"an expr-language call", `{{=sprig.trim(inputs.parameters.x)}}`},
		{"a brace inside an expression", `{{={"a": inputs.parameters.x}}}`},
		{"a tag opened inside a tag", `{{ {{inputs.parameters.x}} }}`},
		{"adjacent tags", `{{a}}{{b}}`},
		{"an empty tag", `{{}}`},
		{"closing braces with no tag", `jq -nc '{status:{phase:"Failed"}}'`},
		{"a stray close before a tag", `}} {{workflow.uid}} {{`},
		{"an unclosed tag", `{{inputs.parameters.x`},
	}
	texts := make([]string, 0, len(cases))
	for _, c := range cases {
		texts = append(texts, c.text)
	}
	input, err := json.Marshal(texts)
	if err != nil {
		t.Fatalf("encode the texts: %v", err)
	}

	gate := filepath.Join("..", "..", "..", "scripts", "check-argo-script-splices.py")
	program := strings.Join([]string{
		"import importlib.util, json, sys",
		`spec = importlib.util.spec_from_file_location("gate", sys.argv[1])`,
		"gate = importlib.util.module_from_spec(spec)",
		`sys.modules["gate"] = gate`,
		"spec.loader.exec_module(gate)",
		"texts = json.load(sys.stdin)",
		"json.dump([[m.group(1).strip() for m in gate.EXPRESSION.finditer(t)] for t in texts], sys.stdout)",
	}, "\n")
	cmd := exec.Command(python, "-I", "-c", program, gate)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("run the gate's grammar: %v\n%s", err, exit.Stderr)
		}
		t.Fatalf("run the gate's grammar: %v", err)
	}
	var theirs [][]string
	if err := json.Unmarshal(out, &theirs); err != nil || len(theirs) != len(cases) {
		t.Fatalf("read the gate's names: %v (%d result(s) for %d text(s)): %s", err, len(theirs), len(cases), out)
	}

	for i, c := range cases {
		ours := []string{}
		for _, tag := range argoExpression.FindAllString(c.text, -1) {
			ours = append(ours, expressionName(tag))
		}
		if !reflect.DeepEqual(ours, theirs[i]) {
			t.Errorf("%s: the harness reads the tags %q, the gate %q", c.what, ours, theirs[i])
		}
	}
	if len(theirs[0]) == 0 {
		t.Error("the gate read no tag in the WorkflowTemplate, so the comparison over it holds vacuously")
	}
}

// assertWholeArguments fails when a stubbed command received part of a payload
// in an argument that does not hold all of it — the shell split it.
func assertWholeArguments(t *testing.T, step string, run stepRun, values map[string]string, attack string) {
	t.Helper()
	payloads := make([]string, 0, len(values))
	for name := range values {
		payloads = append(payloads, payloadFor(name, attack))
	}
	for _, call := range run.calls {
		for _, arg := range call[1:] {
			whole := 0
			for _, p := range payloads {
				whole += strings.Count(arg, p)
			}
			if strings.Count(arg, "PAYLOAD[") != whole || strings.Count(arg, "]PAYLOAD") != whole {
				t.Errorf("step %s handed %s a split value: argument %q holds part of a payload", step, call[0], arg)
			}
		}
	}
}

// assertFlags fails unless the first call of command carries each flag with
// the value given for it.
func assertFlags(t *testing.T, step string, run stepRun, command string, want map[string]string) {
	t.Helper()
	calls := run.commands(command)
	if len(calls) == 0 {
		t.Fatalf("step %s never ran %s", step, command)
	}
	for flag, v := range want {
		if got, ok := flagValue(calls[0], flag); !ok || got != v {
			t.Errorf("step %s runs %s with %s %q, want %q", step, command, flag, got, v)
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
