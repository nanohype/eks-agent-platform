/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
)

// Two workloads the operator creates are collected only after they reach a
// terminal state, and neither reaches one on its own:
//
//	the AgentSandbox session pod   reconcileTTL counts from status.completedAt,
//	                              which the operator writes when it observes the
//	                              pod go terminal
//	the submitted eval run        the run patches the suite's status from inside
//	                              itself; the reconciler does not read the
//	                              workflow's phase, and says so at the point it
//	                              computes the suite phase
//
// So for each, something OUTSIDE this operator has to guarantee the terminal
// state arrives, and something has to report it when it arrives by force rather
// than by the workload finishing.
//
// The invariant these hold: a bound the operator relies on is asserted against
// the artifact that carries it, not against the helper that computes its value.
// A helper returns a number whatever the caller does with it, and the cluster
// enforces only what reached the pod or the workflow — which is the whole point,
// since the operator's absence is the condition under which the bound has to
// work.

// TestTheTTLCannotCollectASessionThatNeverEnds states the coupling both halves
// depend on, in one place, so neither can be read alone.
func TestTheTTLCannotCollectASessionThatNeverEnds(t *testing.T) {
	ctx := context.Background()
	box := &agentsv1alpha1.AgentSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hung", Namespace: ctrlTestNS},
		Spec:       agentsv1alpha1.AgentSandboxSpec{Image: "ghcr.io/acme/sandbox:v1"},
	}

	// A session that never terminates has no completedAt, and the TTL counts
	// from completedAt. The operator's own garbage collection is therefore not
	// merely slow here — it never starts.
	s := sandboxPoolScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &AgentSandboxReconciler{Client: cl, Scheme: s}
	requeue, err := r.reconcileTTL(ctx, box)
	if err != nil {
		t.Fatalf("reconcileTTL on a session with no completedAt: %v", err)
	}
	if requeue != 0 {
		t.Errorf("reconcileTTL asked to requeue in %v for a session that never finished; "+
			"if this ever collects on its own, the reasoning below needs rewriting", requeue)
	}

	// Which is why the ceiling has to be on the pod, where the kubelet enforces
	// it whatever this operator is doing.
	deadline := sandboxCeilingFromCRDDefault(t)
	box.Spec.ActiveDeadlineSeconds = &deadline
	if err := r.ensureSessionPod(ctx, cl, box, newPlatform(ctrlTestPlatform, "team")); err != nil {
		t.Fatalf("ensureSessionPod: %v", err)
	}
	pod := sessionPod(t, cl, box)
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("the session pod carries no activeDeadlineSeconds. Nothing then bounds a hung session: " +
			"the pod holds its node slot and its tenant credentials until a human notices, and the TTL " +
			"above is waiting on a terminal phase that never arrives")
	}
	if got := *pod.Spec.ActiveDeadlineSeconds; got != int64(deadline) {
		t.Errorf("session pod activeDeadlineSeconds = %d, want %d — the ceiling the sandbox declared", got, deadline)
	}
}

// TestSessionCeilingReachesThePodForEveryDeclaration reads the artifact for each
// shape the field can take, rather than the helper that computes it.
func TestSessionCeilingReachesThePodForEveryDeclaration(t *testing.T) {
	secs := func(v int32) *int32 { return &v }
	cases := []struct {
		name string
		in   *int32
		want *int64
	}{
		{"a declared ceiling reaches the pod", secs(600), func() *int64 { v := int64(600); return &v }()},
		{"the value the CRD defaults to is carried through", secs(sandboxCeilingFromCRDDefault(t)), func() *int64 {
			v := int64(sandboxCeilingFromCRDDefault(t))
			return &v
		}()},
		// Kubernetes rejects activeDeadlineSeconds: 0, so "disabled" has to be an
		// absent field rather than a zero one — and absent has to mean absent ON
		// THE POD, not merely nil out of the helper.
		{"an explicit zero leaves the field off the pod", secs(0), nil},
		{"unset leaves the field off the pod", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			box := &agentsv1alpha1.AgentSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: ctrlTestNS},
				Spec:       agentsv1alpha1.AgentSandboxSpec{Image: "ghcr.io/acme/sandbox:v1"},
			}
			box.Spec.ActiveDeadlineSeconds = tc.in

			s := sandboxPoolScheme(t)
			cl := fake.NewClientBuilder().WithScheme(s).Build()
			r := &AgentSandboxReconciler{Client: cl, Scheme: s}
			if err := r.ensureSessionPod(ctx, cl, box, newPlatform(ctrlTestPlatform, "team")); err != nil {
				t.Fatalf("ensureSessionPod: %v", err)
			}

			got := sessionPod(t, cl, box).Spec.ActiveDeadlineSeconds
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("pod carries activeDeadlineSeconds %d; the sandbox declared none", *got)
			case tc.want != nil && got == nil:
				t.Errorf("pod carries no activeDeadlineSeconds; the sandbox declared %d", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("pod activeDeadlineSeconds = %d, want %d", *got, *tc.want)
			}
		})
	}
}

// TestTheShippedSandboxDefaultIsACeiling reads the default out of the generated
// CRD rather than the Go marker, because the CRD is what the apiserver applies
// to a sandbox that declares nothing — which is most of them.
func TestTheShippedSandboxDefaultIsACeiling(t *testing.T) {
	if got := sandboxCeilingFromCRDDefault(t); got <= 0 {
		t.Errorf("the AgentSandbox CRD defaults activeDeadlineSeconds to %d; a sandbox that declares "+
			"nothing then runs unbounded, which is the shape a sandbox must not ship with", got)
	}
}

// submittedRunForms returns both shapes ensureArgoWorkflow emits, with the path
// the run's spec sits at in each. A scheduled suite renders a CronWorkflow whose
// run spec is nested under workflowSpec, so a test that reads only the one-shot
// form asserts nothing about the shape the Forbid reasoning is about.
func submittedRunForms(t *testing.T) []struct {
	name string
	obj  *unstructured.Unstructured
	path []string
} {
	t.Helper()
	r := &EvalReconciler{
		RunnerNamespace:      "eval-runner",
		RunnerServiceAccount: "eval-runner-custom",
		ReportsBucket:        testReportsBucket,
	}
	return []struct {
		name string
		obj  *unstructured.Unstructured
		path []string
	}{
		{"Workflow", submitEvalWorkflow(t, r), []string{"spec"}},
		{"CronWorkflow", submitScheduledEvalRun(t), []string{"spec", "workflowSpec"}},
	}
}

// submitScheduledEvalRun renders the CronWorkflow a scheduled suite produces.
func submitScheduledEvalRun(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	suite, platform, fleet := evalFixtures()
	suite.Spec.Schedule = "0 2 * * *"

	c := fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithObjects(publishedGateway(platform, "chat")).Build()
	r := &EvalReconciler{
		Client:               c,
		RunnerNamespace:      "eval-runner",
		RunnerServiceAccount: "eval-runner-custom",
		ReportsBucket:        testReportsBucket,
	}
	if err := r.ensureArgoWorkflow(context.Background(), suite, platform, fleet); err != nil {
		t.Fatalf("ensureArgoWorkflow: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{
		Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: "CronWorkflow",
	})
	key := client.ObjectKey{Namespace: r.evalRunnerNamespace(), Name: evalWorkflowName(suite)}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("a scheduled suite must render a CronWorkflow: %v", err)
	}
	return got
}

// TestSubmittedRunIsBoundedWithoutTheOperator reads the run the reconciler
// actually submits, in both the forms it emits.
func TestSubmittedRunIsBoundedWithoutTheOperator(t *testing.T) {
	for _, form := range submittedRunForms(t) {
		t.Run(form.name, func(t *testing.T) {
			// Argo's default is no deadline. Under the scheduled form's Forbid
			// policy a run that never finishes is never overtaken and every
			// later run is skipped, while the suite keeps reporting the last
			// completed score; the one-shot form simply runs forever.
			path := append(append([]string{}, form.path...), "activeDeadlineSeconds")
			d, found, err := unstructured.NestedInt64(form.obj.Object, path...)
			if err != nil {
				t.Fatalf("read %v: %v", path, err)
			}
			if !found {
				t.Error("the submitted run carries no activeDeadlineSeconds; a hung run then blocks every " +
					"scheduled run after it while the suite's status still reports the last completed score")
			} else if d <= 0 {
				t.Errorf("activeDeadlineSeconds = %d, which bounds nothing", d)
			}

			// Finished runs are collected by Argo, not by a reconcile, for the
			// same reason the deadline is Argo's: the collection has to happen
			// while nobody is watching.
			//
			// Only on the scheduled form. There the TTL governs the child
			// Workflows a schedule spawns, which the reconciler does not own and
			// will not recreate. On the one-shot form it would delete the object
			// the reconciler owns under a deterministic name, and the next
			// reconcile would find it absent and submit a fresh run — see
			// TestAManualSuiteIsNotCollectedIntoRunningAgain.
			for _, field := range []string{"secondsAfterSuccess", "secondsAfterFailure"} {
				p := append(append([]string{}, form.path...), "ttlStrategy", field)
				ttl, found, err := unstructured.NestedInt64(form.obj.Object, p...)
				if err != nil {
					t.Fatalf("read %v: %v", p, err)
				}
				switch {
				case form.name == "CronWorkflow" && (!found || ttl <= 0):
					t.Errorf("the scheduled run declares no ttlStrategy.%s; its child workflows accumulate "+
						"in the cluster with nothing collecting them", field)
				case form.name == "Workflow" && found:
					t.Errorf("the one-shot run declares ttlStrategy.%s; Argo would delete the object this "+
						"reconciler owns and recreates by name, turning collection into a re-run", field)
				}
			}

			// A failure's pods are the only record of why it failed, so
			// collection is on success only. OnWorkflowCompletion would take the
			// evidence with it.
			p := append(append([]string{}, form.path...), "podGC", "strategy")
			strategy, _, err := unstructured.NestedString(form.obj.Object, p...)
			if err != nil {
				t.Fatalf("read %v: %v", p, err)
			}
			if strategy != "OnWorkflowSuccess" {
				t.Errorf("podGC.strategy = %q, want OnWorkflowSuccess — a completed-run strategy deletes the "+
					"pods of a FAILED run, which are the only record of why it failed", strategy)
			}
		})
	}
}

// TestAManualSuiteIsNotCollectedIntoRunningAgain holds the boundary between
// collecting a finished run and starting another one.
//
// A suite with no schedule renders a Workflow the reconciler owns, names
// deterministically, and CreateOrUpdates on every reconcile — every watch event
// on the suite, and the informer's initial list on operator start. A TTL on that
// object deletes it after it succeeds; the next reconcile finds nothing and
// submits again, against a live model, billed. The CRD documents a suite with no
// schedule as manual only, and that is the sentence this keeps true.
func TestAManualSuiteIsNotCollectedIntoRunningAgain(t *testing.T) {
	r := &EvalReconciler{
		RunnerNamespace:      "eval-runner",
		RunnerServiceAccount: "eval-runner-custom",
		ReportsBucket:        testReportsBucket,
	}
	wf := submitEvalWorkflow(t, r)

	if _, found, err := unstructured.NestedMap(wf.Object, "spec", "ttlStrategy"); err != nil {
		t.Fatalf("read spec.ttlStrategy: %v", err)
	} else if found {
		t.Error("the one-shot run carries a ttlStrategy. Argo deletes the workflow when it expires, the " +
			"next reconcile recreates it by the same name, and a suite documented as manual only runs " +
			"itself on a timer")
	}

	// podGC stays: it collects the run's PODS, which the reconciler does not
	// recreate, so it frees the same resources without touching the object whose
	// absence means "run this".
	if strategy, _, err := unstructured.NestedString(wf.Object, "spec", "podGC", "strategy"); err != nil {
		t.Fatalf("read spec.podGC.strategy: %v", err)
	} else if strategy == "" {
		t.Error("the one-shot run collects neither its workflow nor its pods; a manual suite then leaves " +
			"every run's pods behind for good")
	}
}

// TestATerminatedRunIsReportedOnTheSuite covers the half a deadline alone does
// not deliver.
//
// writeback is the only task that patches EvalSuite.status and it depends on
// score, so a run terminated before scoring patches nothing: the suite keeps the
// phase and score of the last run that finished, carried under a later
// schedule tick. A deadline without this reports a suite that passed.
func TestATerminatedRunIsReportedOnTheSuite(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(readEvalWorkflowTemplate(t)), &doc); err != nil {
		t.Fatalf("parse the WorkflowTemplate: %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)

	handler, _ := spec["onExit"].(string)
	if handler == "" {
		t.Fatal("the WorkflowTemplate declares no onExit; a run that does not reach writeback leaves the " +
			"suite reporting the last completed run's phase and score")
	}

	var tmplNode map[string]any
	var body string
	templates, _ := spec["templates"].([]any)
	for _, raw := range templates {
		tmpl, _ := raw.(map[string]any)
		if tmpl["name"] != handler {
			continue
		}
		tmplNode = tmpl
		script, _ := tmpl["script"].(map[string]any)
		body, _ = script["source"].(string)
	}
	if body == "" {
		t.Fatalf("onExit names %q and no template of that name carries a script; the handler runs nothing", handler)
	}
	// The handler is a pod, not a script: a container beside the script runs on
	// every outcome too, and nothing here reads one. Rather than claim a
	// coverage the file does not have, the template is required to carry no
	// other container.
	for _, key := range []string{"initContainers", "sidecars", "podSpecPatch"} {
		if _, ok := tmplNode[key]; ok {
			t.Errorf("the %s template declares %s. The assertions below read its script alone, so a "+
				"write issued from another container on the same template is invisible to all of them",
				handler, key)
		}
	}

	// What this holds, and what it does not.
	//
	// HOLDS: on a successful run the handler issues no command at all. That is
	// established by RUNNING it — under a shell, with a stub on the path in
	// place of every command that could reach the cluster, and the workflow
	// status set to Succeeded. Nothing here reads the script, so no spelling
	// defeats it: a different verb, a command substitution, a builtin, a trap,
	// or a guard hidden inside a quotation all make the same observable call or
	// fail to suppress one.
	//
	// DOES NOT HOLD: anything outside this script. Other containers on the
	// handler's template, and a podSpecPatch that grafts one on, are checked for
	// existence below because nothing here executes them.
	assertHandlerCalls(t, handler, body, "Succeeded", 0)

	// What follows still reads text, and reads it with comments dropped. These
	// are claims about what the FAILING path writes — which arm it takes and
	// what it clears — and a run of that path observes the calls but not their
	// content, since the stub answers rather than a cluster. A token absent here
	// is a handler that stopped saying something; it is not the success-path
	// property above, which is settled by execution.
	code := shellCode(body)

	// The same harness on the failing path, so a run of it that observes nothing
	// is telling us the stub works rather than that the handler is quiet.
	assertHandlerCalls(t, handler, body, "Failed", 1)

	for _, req := range []struct{ token, why string }{
		{`phase:"Failed"`, "the handler runs on every outcome, so without this it reports nothing about a terminated run"},
		{"kubectl patch evalsuite", "the handler writes to the suite, which is the only object a reader consults"},
		{"kubectl get evalsuite", "the handler asks whether writeback already wrote; without the read it decides from the workflow's phase alone"},
		{"{.status.lastRunAt}", "the read is of the stamp that says who wrote last"},
	} {
		if !strings.Contains(code, req.token) {
			t.Errorf("the %s handler does not carry %q: %s", handler, req.token, req.why)
		}
	}
	// The score of a run that measured nothing is not the previous run's score.
	for _, want := range []string{`lastScore:""`, `lastReportUrl:""`} {
		if !strings.Contains(code, want) {
			t.Errorf("the %s handler leaves %s from the previous run in place, attributing a number to a "+
				"run that produced none", handler, strings.TrimSuffix(want, `:""`))
		}
	}
	// And the other branch: a run that DID measure keeps its numbers, so the
	// handler needs a patch that carries the phase alone.
	if !strings.Contains(code, `{status:{phase:"Failed"}}`) {
		t.Errorf("the %s handler has one patch for both outcomes; a run that scored and then ended badly "+
			"has its measured score blanked by the correction to its phase", handler)
	}
}

// shellCode drops whole-line comments from a shell script so an assertion about
// behaviour cannot be satisfied by prose describing it.
func shellCode(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// sessionPod fetches the pod ensureSessionPod created.
func sessionPod(t *testing.T, cl client.Client, box *agentsv1alpha1.AgentSandbox) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	key := types.NamespacedName{
		Namespace: PlatformNamespace(newPlatform(ctrlTestPlatform, "team")),
		Name:      agentSandboxResourceName(box),
	}
	if err := cl.Get(context.Background(), key, pod); err != nil {
		t.Fatalf("session pod not created: %v", err)
	}
	return pod
}

// sandboxCeilingFromCRDDefault reads spec.activeDeadlineSeconds' default out of
// the generated AgentSandbox CRD — the artifact the apiserver applies, not the
// marker beside the Go field.
func sandboxCeilingFromCRDDefault(t *testing.T) int32 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "agents.nanohype.dev_agentsandboxes.yaml"))
	if err != nil {
		t.Fatalf("read the generated AgentSandbox CRD: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the generated AgentSandbox CRD: %v", err)
	}
	versions, _ := doc["spec"].(map[string]any)["versions"].([]any)
	for _, v := range versions {
		vm, _ := v.(map[string]any)
		if vm["name"] != "v1alpha1" {
			continue
		}
		schema, _ := vm["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
		spec, _ := schema["properties"].(map[string]any)["spec"].(map[string]any)
		field, ok := spec["properties"].(map[string]any)["activeDeadlineSeconds"].(map[string]any)
		if !ok {
			t.Fatal("the generated AgentSandbox CRD declares no spec.activeDeadlineSeconds")
		}
		def, ok := field["default"]
		if !ok {
			t.Fatal("spec.activeDeadlineSeconds carries no default; a sandbox that declares nothing runs unbounded")
		}
		n, ok := def.(float64)
		if !ok {
			t.Fatalf("spec.activeDeadlineSeconds default %v is not a number", def)
		}
		return int32(n)
	}
	t.Fatal("the generated AgentSandbox CRD declares no v1alpha1 schema")
	return 0
}

// TestTheScoreGaugeGoesWithTheScore holds the metric to the same standard as the
// status field beside it.
//
// A terminated run clears status.lastScore because it measured nothing. The
// gauge mirrors that field, and a gauge that keeps its last parseable value
// reports a passing score for the run that produced none — on the surface the
// metric's own doc names as its reader. Status honest and metric stale is the
// same wrong answer reached one layer out.
func TestTheScoreGaugeGoesWithTheScore(t *testing.T) {
	suite, platform, _ := evalFixtures()
	cl := fake.NewClientBuilder().WithScheme(evalScheme(t)).
		WithObjects(suite, publishedGateway(platform, "chat")).WithStatusSubresource(suite).Build()
	r := &EvalReconciler{Client: cl, RunnerNamespace: "eval-runner", ReportsBucket: testReportsBucket}

	suite.Status.LastScore = "0.91"
	if err := r.applyEvalStatus(context.Background(), suite, phaseReady, ""); err != nil {
		t.Fatalf("applyEvalStatus with a score: %v", err)
	}
	if got := testutil.ToFloat64(evalSuiteScore.WithLabelValues(suite.Namespace, suite.Spec.PlatformRef.Name, suite.Name)); got != 0.91 {
		t.Fatalf("gauge = %v after a scored run, want 0.91", got)
	}

	// What the exit handler writes for a run that reached no score.
	suite.Status.LastScore = ""
	if err := r.applyEvalStatus(context.Background(), suite, phaseFailed, ""); err != nil {
		t.Fatalf("applyEvalStatus with no score: %v", err)
	}
	if n := testutil.CollectAndCount(evalSuiteScore); n != 0 {
		t.Errorf("%d score series survived a run that measured nothing; the dashboard still reads the "+
			"last passing score for a suite whose status says it failed", n)
	}
}

// assertHandlerCalls runs the handler's script and counts the commands it issues
// that could reach the cluster.
//
// The script is Argo source, so its parameter references are substituted first;
// then it runs under sh with a directory ahead of PATH holding a stub for every
// such command, each of which records that it was called and exits 0. What the
// handler is observed to do replaces every previous attempt to work it out by
// reading, because a reading can be defeated by how something is written and an
// execution cannot.
func assertHandlerCalls(t *testing.T, handler, source, status string, want int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")

	script := strings.ReplaceAll(source, "{{inputs.parameters.workflow-status}}", status)
	script = regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(script, "probe")

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("make the stub directory: %v", err)
	}
	for _, name := range []string{"kubectl"} {
		stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatalf("write the %s stub: %v", name, err)
		}
	}
	// jq is not a cluster call, but the handler pipes through it and `set -eu`
	// would abort on a missing one.
	jq := "#!/bin/sh\nprintf '{}'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "jq"), []byte(jq), 0o755); err != nil {
		t.Fatalf("write the jq stub: %v", err)
	}

	path := filepath.Join(dir, "handler.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the handler script: %v", err)
	}

	cmd := exec.Command("/bin/sh", path)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s handler failed to run with status %s: %v\n%s", handler, status, err, out)
	}

	var calls int
	if body, rerr := os.ReadFile(log); rerr == nil {
		calls = len(strings.Split(strings.TrimSpace(string(body)), "\n"))
	}
	switch {
	case status == "Succeeded" && calls != want:
		t.Errorf("the %s handler issued %d cluster call(s) on a SUCCEEDED run; a run that wrote its own "+
			"result at writeback must receive none. Calls:\n%s", handler, calls, readFileOrEmpty(log))
	case status != "Succeeded" && calls < want:
		t.Errorf("the %s handler issued no cluster call on a %s run, so this harness cannot tell a quiet "+
			"handler from a stub that never fires", handler, status)
	}
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
