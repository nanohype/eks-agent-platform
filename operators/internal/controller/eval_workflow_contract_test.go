/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/agents/v1alpha1"
	commonv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/common/v1alpha1"
	governancev1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/governance/v1alpha1"
	platformv1alpha1 "github.com/nanohype/eks-agent-platform/operators/api/platform/v1alpha1"
)

// The operator and the WorkflowTemplate are joined by parameter NAMES passed
// through an unstructured object. Neither side can see the other: the operator
// builds a []map[string]any, the template resolves
// `{{workflow.parameters.<name>}}` at run time, and Argo does not reject a
// template parameter nobody supplied — it substitutes the declared default,
// which for this template is a placeholder string.
//
// So an argument the reconciler never sends is not an error anywhere. The run
// executes, uploads to `s3://REPLACE_BY_APPLICATIONSET/...`, and the failure
// surfaces after the model has been billed. These tests assert both ends of
// that seam: what the reconciler puts on the submitted object, and that the
// template consumes exactly those names.

const (
	testReportsBucket = "development-platform-111111111111-us-west-2-eval-reports"
	// The EvalSuite's namespace, deliberately unequal to the tenant name below.
	testSuiteNamespace = "eks-agent-platform"
	testTenant         = "acme"
)

func evalWorkflowTemplatePath(t *testing.T) string {
	t.Helper()
	// operators/internal/controller → repo root → charts/operator/files/...
	return filepath.Join("..", "..", "..", "charts", "operator", "files",
		"eval-runtime", "workflow-template.yaml")
}

// evalFixtures builds a suite whose namespace is deliberately different from
// its tenant name. Those two were conflated — the writeback step patched the
// suite in a namespace named after the tenant — and a fixture that lets them
// be equal cannot tell the fixed code from the broken code.
func evalFixtures() (*governancev1alpha1.EvalSuite, *platformv1alpha1.Platform, *agentsv1alpha1.AgentFleet) {
	suite := &governancev1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: testSuiteNamespace},
		Spec: governancev1alpha1.EvalSuiteSpec{
			PlatformRef:   commonv1alpha1.LocalRef{Name: "analytics"},
			AgentFleetRef: commonv1alpha1.LocalRef{Name: "triage"},
			PassThreshold: "0.85",
			Cases: []governancev1alpha1.EvalCase{
				{Name: "greet", Input: "hi", ExpectContains: []string{"hello"}},
			},
		},
	}
	platform := &platformv1alpha1.Platform{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: testSuiteNamespace},
		Spec:       platformv1alpha1.PlatformSpec{Tenant: testTenant},
	}
	fleet := &agentsv1alpha1.AgentFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "triage", Namespace: testSuiteNamespace},
	}
	return suite, platform, fleet
}

// submitEvalWorkflow runs ensureArgoWorkflow against a fake client and returns
// the object the reconciler actually created.
func submitEvalWorkflow(t *testing.T, r *EvalReconciler) *unstructured.Unstructured {
	t.Helper()
	suite, platform, fleet := evalFixtures()

	c := fake.NewClientBuilder().WithScheme(evalScheme(t)).Build()
	r.Client = c

	if err := r.ensureArgoWorkflow(context.Background(), suite, platform, fleet); err != nil {
		t.Fatalf("ensureArgoWorkflow: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{
		Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: "Workflow",
	})
	key := client.ObjectKey{Namespace: r.evalRunnerNamespace(), Name: evalWorkflowName(suite)}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get submitted workflow %s: %v", key, err)
	}
	return got
}

// TestSubmittedWorkflowSurvivesDeepCopy names the class directly. The whole
// object graph a reconciler hands to unstructured must be JSON-native —
// map[string]interface{}, []interface{}, and scalars. Anything else panics
// apimachinery's deep copy, which no compiler and no `go vet` pass can see,
// because SetNestedField takes `any`.
//
// The eval params slice was []map[string]any. Every EvalSuite reconcile panicked
// the operator, and no test had ever called the function that builds it.
func TestSubmittedWorkflowSurvivesDeepCopy(t *testing.T) {
	obj := submitEvalWorkflow(t, &EvalReconciler{ReportsBucket: testReportsBucket})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the submitted object is not JSON-native: %v", r)
		}
	}()
	runtime.DeepCopyJSONValue(obj.Object)
}

func readEvalWorkflowTemplate(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(evalWorkflowTemplatePath(t))
	if err != nil {
		t.Fatalf("read WorkflowTemplate: %v", err)
	}
	return string(body)
}

// templateArguments parses spec.arguments.parameters rather than searching the
// file for the name. The same names appear again on the writeback step's
// inputs.parameters, so a substring search matches a parameter that is threaded
// between steps but never declared as a workflow argument — which is the one
// thing this is meant to catch. Found by mutation: deleting the declaration
// left the check green.
func templateArguments(t *testing.T, template string) map[string]string {
	t.Helper()
	var doc struct {
		Spec struct {
			Arguments struct {
				Parameters []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"parameters"`
			} `json:"arguments"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(template), &doc); err != nil {
		t.Fatalf("parse WorkflowTemplate: %v", err)
	}
	out := map[string]string{}
	for _, p := range doc.Spec.Arguments.Parameters {
		out[p.Name] = p.Value
	}
	return out
}

func workflowParams(t *testing.T, obj *unstructured.Unstructured) map[string]string {
	t.Helper()
	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "arguments", "parameters")
	if err != nil || !found {
		t.Fatalf("submitted workflow has no spec.arguments.parameters (found=%v err=%v)", found, err)
	}
	out := map[string]string{}
	for _, p := range raw {
		m, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("parameter entry is %T, not a map", p)
		}
		name, _ := m["name"].(string)
		value, _ := m["value"].(string)
		out[name] = value
	}
	return out
}

// TestSubmittedWorkflowCarriesTheValuesTheOperatorHolds asserts the far end of
// the path — the object handed to Argo — rather than the config the operator
// loaded. Three of these values were resolved from SSM into struct fields that
// nothing ever read.
func TestSubmittedWorkflowCarriesTheValuesTheOperatorHolds(t *testing.T) {
	r := &EvalReconciler{
		RunnerNamespace:      "eval-runner",
		RunnerServiceAccount: "eval-runner-custom",
		ReportsBucket:        testReportsBucket,
	}
	obj := submitEvalWorkflow(t, r)
	params := workflowParams(t, obj)

	// The bucket the run uploads to. Left to the WorkflowTemplate's default
	// this is the literal "REPLACE_BY_APPLICATIONSET".
	if got := params["eval-reports-bucket"]; got != testReportsBucket {
		t.Errorf("eval-reports-bucket = %q, want %q — the run would upload to the template's placeholder", got, testReportsBucket)
	}

	// The namespace the writeback step patches. Must be the suite's own, not
	// the tenant name.
	if got := params["suite-namespace"]; got != testSuiteNamespace {
		t.Errorf("suite-namespace = %q, want the EvalSuite's namespace %q", got, testSuiteNamespace)
	}
	if params["suite-namespace"] == params["tenant"] {
		t.Errorf("suite-namespace and tenant are both %q; the fixture is meant to keep them distinct", params["tenant"])
	}

	// The ServiceAccount the Workflow runs under. The chart makes this name a
	// value, so a hardcoded literal here binds Workflows to a ServiceAccount
	// that need not exist.
	sa, found, err := unstructured.NestedString(obj.Object, "spec", "serviceAccountName")
	if err != nil || !found {
		t.Fatalf("submitted workflow has no spec.serviceAccountName (found=%v err=%v)", found, err)
	}
	if sa != "eval-runner-custom" {
		t.Errorf("serviceAccountName = %q, want the configured %q", sa, "eval-runner-custom")
	}
}

// TestReconcilerRefusesWithoutAReportsBucket pins the fail-closed direction. A
// run submitted without a bucket executes every case against a live model and
// only fails at the upload, so the refusal has to happen before submission.
func TestReconcilerRefusesWithoutAReportsBucket(t *testing.T) {
	suite, platform, fleet := evalFixtures()
	c := fake.NewClientBuilder().WithScheme(evalScheme(t)).Build()
	r := &EvalReconciler{Client: c, RunnerNamespace: "eval-runner"}

	err := r.ensureArgoWorkflow(context.Background(), suite, platform, fleet)
	if err == nil {
		t.Fatal("submitted a Workflow with no reports bucket; every upload in that run resolves to s3:///")
	}
	if !strings.Contains(err.Error(), "eval_reports_bucket") {
		t.Errorf("error does not name the parameter an operator would go looking for: %v", err)
	}

	// And nothing was created.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: argoWorkflowsGV.Group, Version: argoWorkflowsGV.Version, Kind: "WorkflowList",
	})
	if err := c.List(context.Background(), list); err == nil && len(list.Items) != 0 {
		t.Errorf("refused but still created %d Workflow(s)", len(list.Items))
	}
}

// TestWorkflowTemplateConsumesEveryParameterTheOperatorSends closes the seam
// from the other side. The operator can send a parameter the template ignores,
// or the template can declare one the operator never sends, and neither is an
// error at any layer — so both directions are checked against the chart file
// the operator's Workflows reference by name.
func TestWorkflowTemplateConsumesEveryParameterTheOperatorSends(t *testing.T) {
	template := readEvalWorkflowTemplate(t)

	r := &EvalReconciler{ReportsBucket: testReportsBucket}
	params := workflowParams(t, submitEvalWorkflow(t, r))
	if len(params) == 0 {
		t.Fatal("the reconciler sent no parameters at all; this check would pass vacuously")
	}

	declared := templateArguments(t, template)
	if len(declared) == 0 {
		t.Fatal("parsed no spec.arguments.parameters out of the WorkflowTemplate; this check would pass vacuously")
	}
	for name := range params {
		if _, ok := declared[name]; !ok {
			t.Errorf("the reconciler sends %q, which the WorkflowTemplate does not declare in spec.arguments.parameters — Argo drops it silently", name)
		}
	}

	// The reverse: a template parameter with no default and no sender resolves
	// to the empty string mid-run.
	for _, required := range []string{"suite-namespace", "eval-reports-bucket", "gateway-url", "suite-name"} {
		if _, ok := params[required]; !ok {
			t.Errorf("the WorkflowTemplate consumes %q but the reconciler never sends it", required)
		}
	}
}

// TestWritebackPatchesTheSuiteNamespace reads the step's own script. The
// parameter can be threaded correctly all the way to the writeback template
// and still be unused by the kubectl invocation inside it.
func TestWritebackPatchesTheSuiteNamespace(t *testing.T) {
	template := readEvalWorkflowTemplate(t)

	if !strings.Contains(template, "--namespace '{{inputs.parameters.suite-namespace}}'") {
		t.Error("the writeback step does not patch in the suite's namespace")
	}
	if strings.Contains(template, "--namespace '{{inputs.parameters.tenant}}'") {
		t.Error("the writeback step patches in a namespace named after the tenant; a tenant name is not a namespace")
	}
}

// TestNoStepDeclaresAnArtifact pins the delivery path to one mechanism. An
// `outputs.artifacts` entry makes Argo push the file to the controller's
// configured artifact repository — of which dev and staging have none, so the
// step fails after the run, on a file the script has already uploaded to S3
// under its own credentials. Two delivery paths for one file, one of which
// cannot work.
func TestNoStepDeclaresAnArtifact(t *testing.T) {
	var doc struct {
		Spec struct {
			Templates []struct {
				Name    string `json:"name"`
				Outputs struct {
					Artifacts []struct {
						Name string `json:"name"`
					} `json:"artifacts"`
				} `json:"outputs"`
			} `json:"templates"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readEvalWorkflowTemplate(t)), &doc); err != nil {
		t.Fatalf("parse WorkflowTemplate: %v", err)
	}
	if len(doc.Spec.Templates) == 0 {
		t.Fatal("parsed no templates; this check would pass vacuously")
	}
	for _, tpl := range doc.Spec.Templates {
		for _, a := range tpl.Outputs.Artifacts {
			t.Errorf("step %q declares outputs.artifacts %q; no artifact repository is configured on dev or staging, and the script already uploads to S3", tpl.Name, a.Name)
		}
	}
}

// TestEveryScriptStepHasTheBinariesItInvokes catches the class directly: a step
// whose image does not carry a command its script runs exits 127 partway
// through, after the earlier steps have already done their work.
func TestEveryScriptStepHasTheBinariesItInvokes(t *testing.T) {
	template := readEvalWorkflowTemplate(t)

	// amazon/aws-cli ships no jq — verified by running the image. Any step
	// that pipes through jq needs the eval-runner image, which carries aws,
	// jq and kubectl together.
	for _, line := range strings.Split(template, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "image:") && strings.Contains(trimmed, "amazon/aws-cli") {
			t.Errorf("step image %q ships no jq, kubectl or node; every step in this template needs at least two of those", trimmed)
		}
	}
}
